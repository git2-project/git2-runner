// Command ci-svc is git2's CI service: it turns pushes into pipelines,
// queues their jobs for runners, and keeps their status, logs and secrets.
//
// Usage:
//
//	ci-svc [serve]
//
// Configuration comes from environment variables; see package config. The
// database schema comes from the git2 repo's migrations: ci-svc checks
// that it is there but never changes it.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/git2-project/git2-runner/app/internal/cisvc/config"
	"github.com/git2-project/git2-runner/app/internal/cisvc/db"
	"github.com/git2-project/git2-runner/app/internal/cisvc/notify"
	"github.com/git2-project/git2-runner/app/internal/cisvc/pipeline"
	"github.com/git2-project/git2-runner/app/internal/cisvc/ratelimit"
	"github.com/git2-project/git2-runner/app/internal/cisvc/runners"
	"github.com/git2-project/git2-runner/app/internal/cisvc/secretbox"
	"github.com/git2-project/git2-runner/app/internal/cisvc/secrets"
	"github.com/git2-project/git2-runner/app/internal/cisvc/server"
	"github.com/git2-project/git2-runner/app/internal/cisvc/source"
)

const (
	// shutdownTimeout is how long in-flight RPCs get to finish on SIGTERM.
	shutdownTimeout = 20 * time.Second
	// reapEvery is how often expired job leases are looked for.
	reapEvery = 15 * time.Second
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(args) > 0 && args[0] != "serve" {
		log.Error("unknown command", slog.String("command", args[0]), slog.String("usage", "ci-svc [serve]"))
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		log.Error("invalid configuration", slog.String("err", err.Error()))
		return 1
	}
	log = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, log, cfg); err != nil {
		log.Error("ci-svc failed", slog.String("err", err.Error()))
		return 1
	}
	return 0
}

// serve runs the startup checks, then serves both ports until ctx is
// canceled.
func serve(ctx context.Context, log *slog.Logger, cfg config.Config) error {
	pool, err := db.Open(ctx, cfg.PostgresDSN, cfg.PostgresMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.CheckSchema(ctx, pool); err != nil {
		return err
	}

	box, err := secretbox.New(cfg.SecretsKey)
	if err != nil {
		return fmt.Errorf("secrets key: %w", err)
	}
	secretSvc := secrets.NewService(pool, box)
	if err := secretSvc.CheckKey(ctx); err != nil {
		return err
	}
	log.Info("database ready", slog.String("secrets_key_id", box.KeyID()))

	rdb := openRedis(ctx, log, cfg)
	if rdb != nil {
		defer func() { _ = rdb.Close() }()
	}
	hub := notify.New(rdb, log)
	go hub.Run(ctx)

	var src source.Source = source.Unavailable{}
	if cfg.DevConfigDir != "" {
		src = source.Dir{Root: cfg.DevConfigDir}
		log.Warn("dev mode: reading .git2-ci.yml from a local directory", slog.String("dir", cfg.DevConfigDir))
	}
	pipelines := pipeline.NewService(pool, src, secretSvc, hub, log, pipeline.DefaultOptions())
	go pipelines.RunReaper(ctx, reapEvery)

	deps := server.Deps{
		Pipelines:     pipelines,
		Runners:       runners.NewService(pool),
		Secrets:       secretSvc,
		RegisterLimit: ratelimit.New(rdb, "register", 10, time.Minute),
		Log:           log,
	}
	var tlsConfig *tls.Config
	if cfg.RunnerTLSCert != "" {
		cert, err := tls.LoadX509KeyPair(cfg.RunnerTLSCert, cfg.RunnerTLSKey)
		if err != nil {
			return fmt.Errorf("load runner TLS certificate: %w", err)
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	} else {
		log.Warn("runner port without TLS: use it only in development or behind a proxy that terminates TLS")
	}

	internal := server.NewInternal(deps, cfg.Dev)
	runnerSrv := server.NewRunner(deps, tlsConfig)
	lc := &net.ListenConfig{}
	internalLis, err := lc.Listen(ctx, "tcp", cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.GRPCAddr, err)
	}
	runnerLis, err := lc.Listen(ctx, "tcp", cfg.RunnerAddr)
	if err != nil {
		_ = internalLis.Close()
		return fmt.Errorf("listen on %s: %w", cfg.RunnerAddr, err)
	}
	log.Info("serving gRPC",
		slog.String("internal_addr", internalLis.Addr().String()),
		slog.String("runner_addr", runnerLis.Addr().String()),
		slog.Bool("runner_tls", tlsConfig != nil),
		slog.Bool("redis", rdb != nil),
		slog.Bool("dev", cfg.Dev))

	served := make(chan error, 2)
	go func() { served <- internal.Serve(internalLis) }()
	go func() { served <- runnerSrv.Serve(runnerLis) }()
	var serveErr error
	select {
	case err := <-served:
		serveErr = fmt.Errorf("gRPC server stopped: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down", slog.String("timeout", shutdownTimeout.String()))
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	runnerSrv.Shutdown(shutdownCtx)
	internal.Shutdown(shutdownCtx)
	log.Info("stopped")
	return serveErr
}

// openRedis connects to the shared Redis. ci-svc works without it, only
// slower, so a missing or unreachable Redis is a warning, not an error;
// go-redis keeps reconnecting in the background.
func openRedis(ctx context.Context, log *slog.Logger, cfg config.Config) *redis.Client {
	if cfg.RedisAddr == "" {
		log.Warn("REDIS_ADDR is not set: wake-ups only reach this replica; run one replica or set REDIS_ADDR")
		return nil
	}
	rdb := redis.NewClient(notify.RedisOptions(cfg.RedisAddr, cfg.RedisPassword))
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		log.Warn("redis is not reachable; falling back to polling until it is", slog.String("err", err.Error()))
	}
	return rdb
}

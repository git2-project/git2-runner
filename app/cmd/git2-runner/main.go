// Command git2-runner runs git2 CI jobs in containers on a build host.
//
// Usage:
//
//	git2-runner register -url https://ci.example.com:9091 -token g2rt_... [-name host] [-labels linux,docker] [-config path] [-force]
//	git2-runner run [-config path]
//	git2-runner version
//
// register trades a one-time registration token (from the console) for a
// runner token and writes the config file. run asks ci-svc for jobs and
// runs them with the Docker daemon from DOCKER_HOST, or the local one.
// The registration token can also come from GIT2_RUNNER_REGISTRATION_TOKEN,
// so it stays out of the shell history.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
	"github.com/git2-project/git2-runner/app/internal/runner"
	"github.com/git2-project/git2-runner/app/internal/runner/docker"
)

// defaultConfig is where the config file lives unless -config or
// GIT2_RUNNER_CONFIG says otherwise.
const defaultConfig = "/etc/git2-runner/config.json"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "register":
		err = register(os.Args[2:], log)
	case "run":
		err = run(os.Args[2:], log)
	case "version":
		fmt.Println("git2-runner", runner.Version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Error("git2-runner failed", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: git2-runner register|run|version [flags]; see -h of each command")
}

func configFlag(fs *flag.FlagSet) *string {
	def := defaultConfig
	if v := os.Getenv("GIT2_RUNNER_CONFIG"); v != "" {
		def = v
	}
	return fs.String("config", def, "path of the config file (GIT2_RUNNER_CONFIG)")
}

func register(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	host, _ := os.Hostname()
	url := fs.String("url", "", "ci-svc runner port, e.g. https://ci.example.com:9091")
	tok := fs.String("token", os.Getenv("GIT2_RUNNER_REGISTRATION_TOKEN"), "one-time registration token (GIT2_RUNNER_REGISTRATION_TOKEN)")
	name := fs.String("name", host, "name shown in the console")
	labels := fs.String("labels", "", "comma-separated labels; jobs whose tags are all among them run here")
	force := fs.Bool("force", false, "replace an existing config file")
	cfgPath := configFlag(fs)
	_ = fs.Parse(args)
	if *url == "" || *tok == "" {
		return errors.New("register needs -url and -token")
	}

	cfg := runner.Config{URL: *url, Name: *name, Docker: runner.DefaultDockerLimits}
	for _, l := range strings.Split(*labels, ",") {
		if l = strings.TrimSpace(l); l != "" {
			cfg.Labels = append(cfg.Labels, l)
		}
	}
	// Check that the file can be written before using up the token.
	if _, err := os.Stat(*cfgPath); err == nil && !*force {
		return fmt.Errorf("%s exists; pass -force to replace it", *cfgPath)
	}

	conn, err := runner.Dial(*url, "")
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := civ1.NewRunnerServiceClient(conn).RegisterRunner(ctx, &civ1.RegisterRunnerRequest{
		RegistrationToken: *tok, Name: cfg.Name, Labels: cfg.Labels, Executor: "docker",
		Version: runner.Version, Platform: runtime.GOOS + "/" + runtime.GOARCH,
	})
	if err != nil {
		return fmt.Errorf("register with ci-svc: %w", err)
	}
	cfg.RunnerID, cfg.Token = resp.GetRunnerId(), resp.GetRunnerToken()
	if err := runner.SaveConfig(*cfgPath, cfg, *force); err != nil {
		return err
	}
	log.Info("runner registered", slog.String("runner_id", cfg.RunnerID), slog.String("config", *cfgPath))
	return nil
}

func run(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := configFlag(fs)
	_ = fs.Parse(args)
	cfg, err := runner.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	exec, err := docker.New(ctx, cfg.Docker, log)
	if err != nil {
		return err
	}
	defer func() { _ = exec.Close() }()
	conn, err := runner.Dial(cfg.URL, cfg.Token)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	log.Info("runner started", slog.String("runner_id", cfg.RunnerID), slog.String("url", cfg.URL),
		slog.Any("labels", cfg.Labels), slog.String("version", runner.Version))
	agent := &runner.Agent{Client: civ1.NewRunnerServiceClient(conn), Executor: exec, Log: log}
	err = agent.Run(ctx)
	log.Info("runner stopped")
	return err
}

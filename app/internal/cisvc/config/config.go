// Package config loads ci-svc's configuration from environment variables.
//
// The database and Redis variables (POSTGRES_DSN, POSTGRES_MAX_CONNS,
// REDIS_ADDR, REDIS_PASSWORD) have the same names and rules as in the git2
// repo, so one .env works for every service. ci-svc's own variables start
// with GIT2_CISVC_; the secrets key is GIT2_CI_SECRETS_KEY.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/git2-project/git2-runner/app/internal/cisvc/secretbox"
)

// Environment variable names read by Load.
const (
	EnvPostgresDSN      = "POSTGRES_DSN"
	EnvPostgresMaxConns = "POSTGRES_MAX_CONNS"
	EnvRedisAddr        = "REDIS_ADDR"
	EnvRedisPassword    = "REDIS_PASSWORD"
	EnvSecretsKey       = "GIT2_CI_SECRETS_KEY" //nolint:gosec // the variable's name, not a credential
	EnvGRPCAddr         = "GIT2_CISVC_GRPC_ADDR"
	EnvRunnerAddr       = "GIT2_CISVC_RUNNER_ADDR"
	EnvRunnerTLSCert    = "GIT2_CISVC_RUNNER_TLS_CERT"
	EnvRunnerTLSKey     = "GIT2_CISVC_RUNNER_TLS_KEY"
	EnvDev              = "GIT2_CISVC_DEV"
	EnvDevConfigDir     = "GIT2_CISVC_DEV_CONFIG_DIR"
	EnvLogLevel         = "GIT2_CISVC_LOG_LEVEL"
)

// Defaults used when a variable is unset.
const (
	DefaultGRPCAddr         = ":9090"
	DefaultRunnerAddr       = ":9091"
	DefaultPostgresMaxConns = 10
)

// Config is the validated runtime configuration of ci-svc. PostgresDSN,
// RedisPassword and SecretsKey are secrets and must never be logged.
type Config struct {
	PostgresDSN      string
	PostgresMaxConns int32
	// RedisAddr is empty when ci-svc runs without Redis (one replica only).
	RedisAddr     string
	RedisPassword string
	// SecretsKey is the 32-byte AES key for CI secrets.
	SecretsKey []byte
	// GRPCAddr is the internal port; RunnerAddr the port for runners.
	GRPCAddr   string
	RunnerAddr string
	// RunnerTLSCert and RunnerTLSKey are PEM files for the runner port.
	// Both empty means plaintext, for development or behind a TLS proxy.
	RunnerTLSCert string
	RunnerTLSKey  string
	// Dev enables developer-only features: gRPC reflection and
	// DevConfigDir.
	Dev bool
	// DevConfigDir, if set, is where ci-svc reads .git2-ci.yml files
	// instead of git-svc: <dir>/<repository_id>/.git2-ci.yml.
	DevConfigDir string
	LogLevel     slog.Level
}

// Load reads and validates the configuration from the process environment.
func Load() (Config, error) {
	return parse(os.LookupEnv)
}

// parse builds a Config from lookup. It reports every invalid or missing
// variable at once, so an operator can fix them in one go. Error messages
// never contain secret values.
func parse(lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{
		GRPCAddr:         DefaultGRPCAddr,
		RunnerAddr:       DefaultRunnerAddr,
		PostgresMaxConns: DefaultPostgresMaxConns,
		LogLevel:         slog.LevelInfo,
	}
	get := func(name string) string {
		v, _ := lookup(name)
		return v
	}
	var errs []error

	if cfg.PostgresDSN = get(EnvPostgresDSN); cfg.PostgresDSN == "" {
		errs = append(errs, fmt.Errorf("%s: required", EnvPostgresDSN))
	}
	if v := get(EnvPostgresMaxConns); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 1 || n > 1000 {
			errs = append(errs, fmt.Errorf("%s: must be a number from 1 to 1000, got %q", EnvPostgresMaxConns, v))
		} else {
			cfg.PostgresMaxConns = int32(n)
		}
	}

	cfg.RedisAddr = get(EnvRedisAddr)
	cfg.RedisPassword = get(EnvRedisPassword)
	if cfg.RedisAddr != "" {
		if _, _, err := net.SplitHostPort(cfg.RedisAddr); err != nil {
			errs = append(errs, fmt.Errorf("%s: must be host:port", EnvRedisAddr))
		}
	}

	if v := get(EnvSecretsKey); v == "" {
		errs = append(errs, fmt.Errorf("%s: required; create one with `openssl rand -base64 32` and keep it with the instance's other secrets", EnvSecretsKey))
	} else if key, err := secretbox.ParseKey(v); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvSecretsKey, err))
	} else {
		cfg.SecretsKey = key
	}

	for _, a := range []struct {
		name string
		dst  *string
	}{{EnvGRPCAddr, &cfg.GRPCAddr}, {EnvRunnerAddr, &cfg.RunnerAddr}} {
		if v := get(a.name); v != "" {
			*a.dst = v
		}
		if _, _, err := net.SplitHostPort(*a.dst); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.name, err))
		}
	}

	cfg.RunnerTLSCert, cfg.RunnerTLSKey = get(EnvRunnerTLSCert), get(EnvRunnerTLSKey)
	if (cfg.RunnerTLSCert == "") != (cfg.RunnerTLSKey == "") {
		errs = append(errs, fmt.Errorf("%s and %s: set both or neither", EnvRunnerTLSCert, EnvRunnerTLSKey))
	}

	if v := get(EnvDev); v != "" {
		dev, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: must be a boolean, got %q", EnvDev, v))
		}
		cfg.Dev = dev
	}
	if v := get(EnvDevConfigDir); v != "" {
		switch {
		case !cfg.Dev:
			errs = append(errs, fmt.Errorf("%s: only allowed with %s=true", EnvDevConfigDir, EnvDev))
		case !filepath.IsAbs(v):
			errs = append(errs, fmt.Errorf("%s: must be an absolute path, got %q", EnvDevConfigDir, v))
		default:
			cfg.DevConfigDir = filepath.Clean(v)
		}
	}

	if v := get(EnvLogLevel); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvLogLevel, err))
		}
	}
	return cfg, errors.Join(errs...)
}

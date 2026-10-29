// Package runner is the agent that runs on a build host: it registers with
// ci-svc, asks for jobs, runs each one through an Executor, streams the
// log with secrets masked, and reports the result.
//
// The runner holds nothing but its own token. It never gets the database
// DSN, the Redis address, the secrets key or bucket credentials; a job's
// secrets reach it only while that job runs, and are never written to
// disk (plan §3, R-05).
package runner

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
)

// Version is the runner's version, reported to ci-svc. Set at build time
// with -ldflags "-X .../internal/runner.Version=...".
var Version = "0.1.0-dev"

// Config is the runner's config file. It holds the runner token, so it
// is written with permissions 0600.
type Config struct {
	// URL of ci-svc's runner port: https://host:9091, or http://host:9091
	// without TLS (development only).
	URL      string   `json:"url"`
	RunnerID string   `json:"runner_id"`
	Token    string   `json:"token"`
	Name     string   `json:"name"`
	Labels   []string `json:"labels,omitempty"`
	// Docker limits for every job container.
	Docker DockerLimits `json:"docker"`
}

// DockerLimits bound what one job container can use.
type DockerLimits struct {
	MemoryMB  int64   `json:"memory_mb"`
	CPUs      float64 `json:"cpus"`
	PidsLimit int64   `json:"pids_limit"`
}

// DefaultDockerLimits are used when the config file has none.
var DefaultDockerLimits = DockerLimits{MemoryMB: 2048, CPUs: 2, PidsLimit: 1024}

// LoadConfig reads a config file written by SaveConfig.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	if c.URL == "" || c.Token == "" {
		return Config{}, fmt.Errorf("config %s has no url or token: run `git2-runner register` first", path)
	}
	if c.Docker == (DockerLimits{}) {
		c.Docker = DefaultDockerLimits
	}
	return c, nil
}

// SaveConfig writes c to path with permissions 0600. It refuses to
// overwrite an existing file unless force is set.
func SaveConfig(path string, c Config, force bool) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !force {
		flags |= os.O_EXCL
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s exists; pass -force to replace it", path)
	}
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("write config: %w", err)
	}
	return f.Close()
}

// Dial connects to ci-svc's runner port. token may be empty, for
// RegisterRunner.
func Dial(rawURL, token string) (*grpc.ClientConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("ci-svc URL must look like https://host:9091, got %q", rawURL)
	}
	creds := insecure.NewCredentials()
	if u.Scheme == "https" {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	if token != "" {
		opts = append(opts, grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any,
			cc *grpc.ClientConn, invoker grpc.UnaryInvoker, callOpts ...grpc.CallOption) error {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
			return invoker(ctx, method, req, reply, cc, callOpts...)
		}))
	}
	return grpc.NewClient("dns:///"+u.Host, opts...)
}

// Spec is one job as an Executor sees it.
type Spec struct {
	JobID  string
	Image  string
	Script string
	// Env holds NAME=value entries, secrets included. Executors pass it to
	// the job without writing it to disk or to a command line.
	Env []string
}

// Result is how a job ended. Failure is set when the job failed for a
// reason other than its script's exit code.
type Result struct {
	ExitCode int
	Failure  civ1.JobFailureReason
}

// Executor runs one job's script and writes its output to out. It returns
// when the script has exited or ctx ends; then the job is stopped and
// cleaned up.
type Executor interface {
	Run(ctx context.Context, spec Spec, out io.Writer) Result
}

// BuildScript turns .git2-ci.yml script lines into one /bin/sh script
// that echoes each command before running it and stops at the first
// command that fails, like `sh -e`.
func BuildScript(lines []string) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	for _, line := range lines {
		b.WriteString("printf '%s\\n' " + shellQuote("\x1b[32;1m$ "+line+"\x1b[0m") + "\n")
		b.WriteString(line + "\n")
	}
	return b.String()
}

// shellQuote quotes s for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

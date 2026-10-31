//go:build integration

package docker

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/client"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
	"github.com/git2-project/git2-runner/app/internal/runner"
)

// These tests need a Docker daemon (DOCKER_HOST, or the local one) that
// can pull alpine.
const image = "alpine:3.20"

type buffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func newExecutor(t *testing.T) *Executor {
	t.Helper()
	e, err := New(t.Context(), runner.DefaultDockerLimits, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Skipf("no Docker daemon: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func TestRun(t *testing.T) {
	e := newExecutor(t)
	jobID := uuid.NewString()
	var out buffer
	res := e.Run(t.Context(), runner.Spec{
		JobID: jobID,
		Image: image,
		Script: runner.BuildScript([]string{
			`echo "greeting=$GREETING"`,
			`printf '%s' "$KEY" | wc -l`,
			`pwd`,
			`test ! -e /var/run/docker.sock && echo no-socket`,
			`grep -q 'NoNewPrivs:.*1' /proc/self/status && echo no-new-privs`,
			`exit 3`,
			`echo never`,
		}),
		Env: []string{"GREETING=hello world", "KEY=line1\nline2\nline3"},
	}, &out)
	if res.ExitCode != 3 || res.Failure != civ1.JobFailureReason_JOB_FAILURE_REASON_UNSPECIFIED {
		t.Fatalf("result = %+v\n%s", res, out.String())
	}
	log := out.String()
	for _, want := range []string{"$ echo", "greeting=hello world", "2\n", "/workspace", "no-socket", "no-new-privs"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "\nnever") {
		t.Errorf("the script went on after a failing command:\n%s", log)
	}

	// Nothing of the job is left on the daemon.
	ctx := t.Context()
	name := "git2-job-" + jobID
	if _, err := e.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{}); err == nil {
		t.Error("container left behind")
	}
	if _, err := e.cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{}); err == nil {
		t.Error("network left behind")
	}
	if _, err := e.cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{}); err == nil {
		t.Error("volume left behind")
	}
}

func TestRunTimeout(t *testing.T) {
	e := newExecutor(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	start := time.Now()
	var out buffer
	res := e.Run(ctx, runner.Spec{JobID: uuid.NewString(), Image: image, Script: "echo started; sleep 60"}, &out)
	if time.Since(start) > 20*time.Second {
		t.Fatalf("the job was not stopped at the deadline (%s)", time.Since(start))
	}
	if res.ExitCode != -1 || ctx.Err() == nil {
		t.Fatalf("result = %+v, ctx = %v", res, ctx.Err())
	}
}

func TestRunMissingImage(t *testing.T) {
	e := newExecutor(t)
	var out buffer
	res := e.Run(t.Context(), runner.Spec{JobID: uuid.NewString(), Image: "git2-test/does-not-exist:never", Script: "true"}, &out)
	if res.Failure != civ1.JobFailureReason_JOB_FAILURE_REASON_IMAGE_PULL_FAILURE {
		t.Fatalf("result = %+v\n%s", res, out.String())
	}
}

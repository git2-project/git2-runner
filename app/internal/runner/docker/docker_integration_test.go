//go:build integration

package docker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
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
	return newExecutorFor(t, uuid.NewString())
}

func newExecutorFor(t *testing.T, runnerID string) *Executor {
	t.Helper()
	e, err := New(t.Context(), runnerID, runner.DefaultDockerLimits, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

// TestRunLabelsWithRunnerID checks that everything a running job created
// carries the runner's ID, which is how RemoveLeftovers finds it.
func TestRunLabelsWithRunnerID(t *testing.T) {
	runnerID := uuid.NewString()
	e := newExecutorFor(t, runnerID)
	jobID := uuid.NewString()
	name := "git2-job-" + jobID
	done := make(chan runner.Result, 1)
	go func() {
		var out buffer
		done <- e.Run(t.Context(), runner.Spec{JobID: jobID, Image: image, Script: "sleep 5"}, &out)
	}()

	ctx := t.Context()
	deadline := time.Now().Add(60 * time.Second)
	for {
		c, err := e.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
		if err == nil && c.Container.State != nil && c.Container.State.Running {
			if got := c.Container.Config.Labels; got[LabelRunnerID] != runnerID || got[LabelJobID] != jobID {
				t.Errorf("container labels = %v", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job container never ran: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	n, err := e.cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err != nil || n.Network.Labels[LabelRunnerID] != runnerID {
		t.Errorf("network: %v, labels %v", err, n.Network.Labels)
	}
	v, err := e.cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err != nil || v.Volume.Labels[LabelRunnerID] != runnerID {
		t.Errorf("volume: %v, labels %v", err, v.Volume.Labels)
	}
	if res := <-done; res.ExitCode != 0 {
		t.Fatalf("result = %+v", res)
	}
}

// leftovers creates what a job of runnerID leaves behind when its runner
// dies: a running container, its network and its volume, named like a
// job's. It removes them when the test ends, if they are still there.
func leftovers(t *testing.T, e *Executor, runnerID string) string {
	t.Helper()
	ctx := t.Context()
	if err := e.ensureImage(ctx, image, io.Discard); err != nil {
		t.Fatal(err)
	}
	name := "git2-job-" + uuid.NewString()
	labels := map[string]string{LabelJobID: strings.TrimPrefix(name, "git2-job-"), LabelRunnerID: runnerID}
	if _, err := e.cli.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Labels: labels}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{Driver: "bridge", Labels: labels}); err != nil {
		t.Fatal(err)
	}
	created, err := e.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:   name,
		Config: &container.Config{Image: image, Cmd: []string{"sleep", "300"}, Labels: labels},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(name),
			Mounts:      []mount.Mount{{Type: mount.TypeVolume, Source: name, Target: "/workspace"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = e.cli.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true})
		_, _ = e.cli.NetworkRemove(ctx, name, client.NetworkRemoveOptions{})
		_, _ = e.cli.VolumeRemove(ctx, name, client.VolumeRemoveOptions{Force: true})
	})
	return name
}

func TestRemoveLeftovers(t *testing.T) {
	runnerA, runnerB := uuid.NewString(), uuid.NewString()
	a := newExecutorFor(t, runnerA)
	mine := leftovers(t, a, runnerA)
	theirs := leftovers(t, a, runnerB) // another runner on the same daemon

	// Runner A starts again after a crash.
	restarted := newExecutorFor(t, runnerA)
	left, err := restarted.RemoveLeftovers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := Leftovers{Containers: []string{mine}, Networks: []string{mine}, Volumes: []string{mine}}
	if fmt.Sprint(left) != fmt.Sprint(want) {
		t.Fatalf("removed %+v, want %+v", left, want)
	}

	ctx := t.Context()
	if _, err := a.cli.ContainerInspect(ctx, mine, client.ContainerInspectOptions{}); err == nil {
		t.Error("runner A's container is still there")
	}
	if _, err := a.cli.VolumeInspect(ctx, mine, client.VolumeInspectOptions{}); err == nil {
		t.Error("runner A's volume is still there")
	}
	c, err := a.cli.ContainerInspect(ctx, theirs, client.ContainerInspectOptions{})
	if err != nil || c.Container.State == nil || !c.Container.State.Running {
		t.Errorf("runner B's running container was touched: %v", err)
	}
	if _, err := a.cli.NetworkInspect(ctx, theirs, client.NetworkInspectOptions{}); err != nil {
		t.Errorf("runner B's network was removed: %v", err)
	}
	if _, err := a.cli.VolumeInspect(ctx, theirs, client.VolumeInspectOptions{}); err != nil {
		t.Errorf("runner B's volume was removed: %v", err)
	}

	// A second start finds nothing.
	if left, err := restarted.RemoveLeftovers(ctx); err != nil || !left.Empty() {
		t.Fatalf("second start: %+v, %v", left, err)
	}
	// Without a runner ID nothing can be told apart, so nothing is removed.
	if _, err := newExecutorFor(t, "").RemoveLeftovers(ctx); err == nil {
		t.Fatal("RemoveLeftovers without a runner ID did not refuse")
	}
}

// Package docker runs jobs in containers through the Docker Engine API.
//
// Each job gets its own container, bridge network and workspace volume,
// all removed when the job ends. Containers are never privileged, never
// see the Docker socket, run with no-new-privileges and with memory, CPU
// and process limits (plan §5, R-05). Environment variables, secrets
// included, travel in the API request: never on a command line or disk.
//
// The daemon is the one in DOCKER_HOST, or the local one. It may be a
// different machine (Docker-in-Docker in the devcontainer), so jobs never
// bind-mount runner paths.
//
// Everything a job creates carries two labels: the job's ID and the
// runner's ID. If the runner or its host dies mid-job, the container,
// network and volume stay on the daemon; the next start of the same runner
// removes them (RemoveLeftovers). Several runners may share one daemon, so
// a runner only ever removes what carries its own ID.
package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
	"github.com/git2-project/git2-runner/app/internal/runner"
)

// workdir is where the job's workspace volume is mounted and its script
// starts.
const workdir = "/workspace"

// cleanupTimeout bounds removing a job's container, network and volume.
const cleanupTimeout = 30 * time.Second

// Labels on everything the executor creates.
const (
	// LabelJobID holds the job's ID.
	LabelJobID = "io.git2.job-id"
	// LabelRunnerID holds the ID of the runner that ran the job.
	LabelRunnerID = "io.git2.runner-id"
)

// Executor runs jobs on one Docker daemon.
type Executor struct {
	cli      *client.Client
	runnerID string
	limits   runner.DockerLimits
	log      *slog.Logger
}

// New connects to the daemon from the environment (DOCKER_HOST and
// friends) and checks that it answers. The client negotiates the API
// version with the daemon. runnerID labels everything the executor
// creates, so RemoveLeftovers can find it after a crash.
func New(ctx context.Context, runnerID string, limits runner.DockerLimits, log *slog.Logger) (*Executor, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := cli.Ping(pingCtx, client.PingOptions{NegotiateAPIVersion: true}); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("reach the Docker daemon: %w", err)
	}
	return &Executor{cli: cli, runnerID: runnerID, limits: limits, log: log}, nil
}

// Close releases the client.
func (e *Executor) Close() error { return e.cli.Close() }

// Run implements runner.Executor.
func (e *Executor) Run(ctx context.Context, spec runner.Spec, out io.Writer) runner.Result {
	name := "git2-job-" + spec.JobID
	labels := map[string]string{LabelJobID: spec.JobID, LabelRunnerID: e.runnerID}
	systemFailure := func(what string, err error) runner.Result {
		if ctx.Err() != nil {
			return runner.Result{ExitCode: -1}
		}
		e.log.Error("docker executor", slog.String("job_id", spec.JobID), slog.String("step", what), slog.String("err", err.Error()))
		fmt.Fprintf(out, "\x1b[31;1mERROR: the runner could not %s.\x1b[0m\n", what)
		return runner.Result{ExitCode: -1, Failure: civ1.JobFailureReason_JOB_FAILURE_REASON_RUNNER_SYSTEM_FAILURE}
	}

	if err := e.ensureImage(ctx, spec.Image, out); err != nil {
		if ctx.Err() != nil {
			return runner.Result{ExitCode: -1}
		}
		fmt.Fprintf(out, "\x1b[31;1mERROR: could not pull image %s: %v\x1b[0m\n", spec.Image, err)
		return runner.Result{ExitCode: -1, Failure: civ1.JobFailureReason_JOB_FAILURE_REASON_IMAGE_PULL_FAILURE}
	}

	if _, err := e.cli.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Labels: labels}); err != nil {
		return systemFailure("create the workspace volume", err)
	}
	defer e.cleanup(ctx, func(c context.Context) error {
		_, err := e.cli.VolumeRemove(c, name, client.VolumeRemoveOptions{Force: true})
		return err
	})
	if _, err := e.cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{Driver: "bridge", Labels: labels}); err != nil {
		return systemFailure("create the job network", err)
	}
	defer e.cleanup(ctx, func(c context.Context) error {
		_, err := e.cli.NetworkRemove(c, name, client.NetworkRemoveOptions{})
		return err
	})

	yes := true
	pids := e.limits.PidsLimit
	created, err := e.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:        spec.Image,
			Entrypoint:   []string{"/bin/sh"},
			Cmd:          []string{"-c", spec.Script},
			Env:          spec.Env,
			WorkingDir:   workdir,
			Labels:       labels,
			AttachStdout: true,
			AttachStderr: true,
		},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(name),
			Mounts:      []mount.Mount{{Type: mount.TypeVolume, Source: name, Target: workdir}},
			Init:        &yes,
			SecurityOpt: []string{"no-new-privileges"},
			Resources: container.Resources{
				Memory:    e.limits.MemoryMB << 20,
				NanoCPUs:  int64(e.limits.CPUs * 1e9),
				PidsLimit: &pids,
			},
		},
	})
	if err != nil {
		return systemFailure("create the job container", err)
	}
	defer e.cleanup(ctx, func(c context.Context) error {
		_, err := e.cli.ContainerRemove(c, created.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		return err
	})

	attached, err := e.cli.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return systemFailure("attach to the job container", err)
	}
	defer attached.Close()
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = stdcopy.StdCopy(out, out, attached.Reader)
	}()

	// Ask for the exit status before starting, so a quick job cannot exit
	// unseen.
	wait := e.cli.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})
	if _, err := e.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return systemFailure("start the job container", err)
	}

	select {
	case res := <-wait.Result:
		// Output may still be in flight after the exit.
		select {
		case <-copied:
		case <-time.After(5 * time.Second):
		}
		if res.Error != nil && res.Error.Message != "" {
			return systemFailure("wait for the job container", errors.New(res.Error.Message))
		}
		return runner.Result{ExitCode: int(res.StatusCode)}
	case err := <-wait.Error:
		if ctx.Err() != nil {
			// Timeout or cancel: the deferred remove kills the container.
			return runner.Result{ExitCode: -1}
		}
		return systemFailure("wait for the job container", err)
	}
}

// Leftovers are the names of what RemoveLeftovers removed.
type Leftovers struct {
	Containers, Networks, Volumes []string
}

// Empty reports whether nothing was removed.
func (l Leftovers) Empty() bool {
	return len(l.Containers)+len(l.Networks)+len(l.Volumes) == 0
}

// RemoveLeftovers removes every container, network and volume that carries
// this runner's ID: what jobs left behind when the runner or its host died.
// Call it at startup, before the first job. Containers go first, because
// they hold the networks and volumes. A failure on one item does not stop
// the others; the error lists them all.
//
// Two processes must never run with the same runner ID on one daemon: each
// would remove the other's running jobs at startup.
func (e *Executor) RemoveLeftovers(ctx context.Context) (Leftovers, error) {
	var left Leftovers
	if e.runnerID == "" {
		return left, errors.New("no runner ID: leftovers cannot be told apart from other runners'")
	}
	mine := client.Filters{}.Add("label", LabelRunnerID+"="+e.runnerID)
	var errs []error
	removed := func(what, name string, err error, into *[]string) {
		if err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove %s %s: %w", what, name, err))
			return
		}
		*into = append(*into, name)
	}

	containers, err := e.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: mine})
	if err != nil {
		return left, fmt.Errorf("list containers: %w", err)
	}
	for _, c := range containers.Items {
		name := c.ID
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		_, err := e.cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		removed("container", name, err, &left.Containers)
	}

	networks, err := e.cli.NetworkList(ctx, client.NetworkListOptions{Filters: mine})
	if err != nil {
		return left, errors.Join(append(errs, fmt.Errorf("list networks: %w", err))...)
	}
	for _, n := range networks.Items {
		_, err := e.cli.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{})
		removed("network", n.Name, err, &left.Networks)
	}

	volumes, err := e.cli.VolumeList(ctx, client.VolumeListOptions{Filters: mine})
	if err != nil {
		return left, errors.Join(append(errs, fmt.Errorf("list volumes: %w", err))...)
	}
	for _, v := range volumes.Items {
		_, err := e.cli.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{Force: true})
		removed("volume", v.Name, err, &left.Volumes)
	}
	return left, errors.Join(errs...)
}

// ensureImage pulls the image unless the daemon already has it.
func (e *Executor) ensureImage(ctx context.Context, image string, out io.Writer) error {
	if _, err := e.cli.ImageInspect(ctx, image); err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return err
	}
	fmt.Fprintf(out, "Pulling image %s...\n", image)
	resp, err := e.cli.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Close() }()
	for msg, err := range resp.JSONMessages(ctx) {
		if err != nil {
			return err
		}
		if msg.Error != nil {
			return errors.New(msg.Error.Message)
		}
	}
	return nil
}

// cleanup runs remove with its own timeout, even after ctx ended, and
// logs failures: a leftover container must not fail the job.
func (e *Executor) cleanup(ctx context.Context, remove func(context.Context) error) {
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := remove(c); err != nil && !errdefs.IsNotFound(err) {
		e.log.Warn("docker cleanup", slog.String("err", err.Error()))
	}
}

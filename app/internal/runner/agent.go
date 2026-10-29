package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
)

// Agent timings. The heartbeat must come well within ci-svc's 60-second
// lease.
const (
	requestTimeout   = 60 * time.Second
	heartbeatEvery   = 15 * time.Second
	reportTimeout    = 10 * time.Second
	reportAttempts   = 6
	maxBackoff       = 30 * time.Second
	logFlushOnFinish = 30 * time.Second
)

// ErrUnauthenticated means ci-svc no longer accepts the runner's token:
// the runner was deleted and must be registered again.
var ErrUnauthenticated = errors.New("ci-svc rejected the runner token; register the runner again")

// Agent asks ci-svc for jobs and runs them one at a time.
type Agent struct {
	Client   civ1.RunnerServiceClient
	Executor Executor
	Log      *slog.Logger
}

// Run asks for jobs until ctx ends. A job that has started is finished
// even after ctx ends, so a SIGTERM never leaves a job half-reported.
func (a *Agent) Run(ctx context.Context) error {
	backoff := time.Second
	for ctx.Err() == nil {
		reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		resp, err := a.Client.RequestJob(reqCtx, &civ1.RequestJobRequest{})
		cancel()
		switch {
		case ctx.Err() != nil:
			return nil
		case status.Code(err) == codes.Unauthenticated:
			return ErrUnauthenticated
		case err != nil:
			a.Log.Warn("ask ci-svc for a job", slog.String("err", err.Error()), slog.Duration("retry_in", backoff))
			sleep(ctx, backoff)
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = time.Second
		if job := resp.GetJob(); job != nil {
			a.RunJob(context.WithoutCancel(ctx), job)
		}
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// Causes of a job's context ending early.
var (
	errCanceled = errors.New("the job was canceled")
	errLost     = errors.New("ci-svc no longer assigns the job to this runner")
)

// RunJob runs one job and reports its result.
func (a *Agent) RunJob(ctx context.Context, job *civ1.JobAssignment) {
	log := a.Log.With(slog.String("job_id", job.GetJobId()), slog.String("job", job.GetName()))
	log.Info("job started", slog.String("image", job.GetImage()))

	var env, secrets []string
	for _, v := range job.GetVariables() {
		env = append(env, v.GetName()+"="+v.GetValue())
		if v.GetMasked() {
			secrets = append(secrets, v.GetValue())
		}
	}
	out := newLogShipper(a.Client, job.GetJobId(), secrets)
	host, _ := os.Hostname()
	fmt.Fprintf(out, "\x1b[1mRunning job %s on git2-runner %s (%s), image %s\x1b[0m\n",
		job.GetName(), Version, host, job.GetImage())

	jobCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timeout := job.GetTimeout().AsDuration()
	runCtx, cancelTimeout := context.WithTimeout(jobCtx, timeout)
	defer cancelTimeout()

	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		a.heartbeat(runCtx, job.GetJobId(), cancel)
	}()

	res := a.Executor.Run(runCtx, Spec{
		JobID: job.GetJobId(), Image: job.GetImage(), Script: BuildScript(job.GetScript()), Env: env,
	}, out)
	cause := context.Cause(jobCtx)
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded) && cause == nil
	cancelTimeout()
	<-hbDone

	req := &civ1.UpdateJobRequest{JobId: job.GetJobId()}
	switch {
	case errors.Is(cause, errLost):
		log.Warn("job taken away by ci-svc; not reporting it")
		out.Close(context.Background())
		return
	case errors.Is(cause, errCanceled):
		fmt.Fprintln(out, "\n\x1b[31;1mThe job was canceled.\x1b[0m")
		req.Status, req.FailureReason = civ1.JobStatus_JOB_STATUS_FAILED, civ1.JobFailureReason_JOB_FAILURE_REASON_RUNNER_SYSTEM_FAILURE
	case timedOut:
		fmt.Fprintf(out, "\n\x1b[31;1mThe job timed out after %s.\x1b[0m\n", timeout)
		req.Status, req.FailureReason = civ1.JobStatus_JOB_STATUS_FAILED, civ1.JobFailureReason_JOB_FAILURE_REASON_TIMEOUT
	case res.Failure != civ1.JobFailureReason_JOB_FAILURE_REASON_UNSPECIFIED:
		req.Status, req.FailureReason = civ1.JobStatus_JOB_STATUS_FAILED, res.Failure
	case res.ExitCode == 0:
		fmt.Fprintln(out, "\n\x1b[32;1mJob succeeded\x1b[0m")
		req.Status, req.ExitCode = civ1.JobStatus_JOB_STATUS_SUCCESS, ptr(int32(0))
	default:
		fmt.Fprintf(out, "\n\x1b[31;1mJob failed: exit code %d\x1b[0m\n", res.ExitCode)
		req.Status, req.FailureReason = civ1.JobStatus_JOB_STATUS_FAILED, civ1.JobFailureReason_JOB_FAILURE_REASON_SCRIPT_FAILURE
		req.ExitCode = ptr(int32(max(-1, min(res.ExitCode, 255)))) //nolint:gosec // clamped to the range of exit codes
	}

	// The whole log goes before the final status, so log followers see
	// every byte before the stream ends.
	flushCtx, cancelFlush := context.WithTimeout(ctx, logFlushOnFinish)
	out.Close(flushCtx)
	cancelFlush()
	a.report(ctx, log, req)
}

// heartbeat extends the job's lease every heartbeatEvery and cancels the
// job when ci-svc says it was canceled or is no longer this runner's.
func (a *Agent) heartbeat(ctx context.Context, jobID string, cancel context.CancelCauseFunc) {
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		hbCtx, done := context.WithTimeout(ctx, reportTimeout)
		resp, err := a.Client.UpdateJob(hbCtx, &civ1.UpdateJobRequest{JobId: jobID, Status: civ1.JobStatus_JOB_STATUS_RUNNING})
		done()
		switch status.Code(err) {
		case codes.OK:
			if resp.GetCancelRequested() {
				cancel(errCanceled)
				return
			}
		case codes.NotFound, codes.FailedPrecondition, codes.Unauthenticated:
			cancel(errLost)
			return
		default:
			// ci-svc unreachable: keep running; the lease covers a few
			// missed heartbeats.
			a.Log.Warn("heartbeat failed", slog.String("job_id", jobID), slog.String("err", err.Error()))
		}
	}
}

// report sends the final status, retrying while ci-svc is unreachable.
func (a *Agent) report(ctx context.Context, log *slog.Logger, req *civ1.UpdateJobRequest) {
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		rCtx, cancel := context.WithTimeout(ctx, reportTimeout)
		_, err := a.Client.UpdateJob(rCtx, req)
		cancel()
		code := status.Code(err)
		switch {
		case err == nil:
			log.Info("job finished", slog.String("status", req.GetStatus().String()))
			return
		case code == codes.NotFound || code == codes.FailedPrecondition || code == codes.InvalidArgument || attempt == reportAttempts:
			log.Error("report job result", slog.String("err", err.Error()))
			return
		}
		sleep(ctx, backoff)
		backoff = min(backoff*2, maxBackoff)
	}
}

func ptr[T any](v T) *T { return &v }

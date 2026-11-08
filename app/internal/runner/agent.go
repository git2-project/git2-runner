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

// timings are the agent's intervals and timeouts. Tests shorten them.
type timings struct {
	request    time.Duration // deadline of one RequestJob long poll
	heartbeat  time.Duration // between two heartbeats
	call       time.Duration // deadline of one UpdateJob or AppendLog
	backoff    time.Duration // first wait after a failed call
	maxBackoff time.Duration // longest wait after failed calls
	logFlush   time.Duration // how long the rest of a log may take to send when the job ends
	flushEvery time.Duration // between two batches of log
}

// defaultTimings are the production values. The heartbeat must come well
// within ci-svc's 60-second lease.
var defaultTimings = timings{
	request:    60 * time.Second,
	heartbeat:  15 * time.Second,
	call:       10 * time.Second,
	backoff:    time.Second,
	maxBackoff: 30 * time.Second,
	logFlush:   30 * time.Second,
	flushEvery: 500 * time.Millisecond,
}

// reportAttempts is how often the final status is sent before giving up.
const reportAttempts = 6

// ErrUnauthenticated means ci-svc no longer accepts the runner's token:
// the runner was deleted and must be registered again.
var ErrUnauthenticated = errors.New("ci-svc rejected the runner token; register the runner again")

// Agent asks ci-svc for jobs and runs them one at a time.
type Agent struct {
	Client   civ1.RunnerServiceClient
	Executor Executor
	Log      *slog.Logger

	timings *timings // nil means defaultTimings
}

func (a *Agent) timing() *timings {
	if a.timings != nil {
		return a.timings
	}
	return &defaultTimings
}

// Run asks for jobs until ctx ends. A job that has started is finished
// even after ctx ends, so a SIGTERM never leaves a job half-reported.
func (a *Agent) Run(ctx context.Context) error {
	tm := a.timing()
	backoff := tm.backoff
	for ctx.Err() == nil {
		reqCtx, cancel := context.WithTimeout(ctx, tm.request)
		resp, err := a.Client.RequestJob(reqCtx, &civ1.RequestJobRequest{})
		cancel()
		switch {
		case err == nil:
			backoff = tm.backoff
			// Run a job even if ctx ended while its reply was on the way:
			// ci-svc has leased it, and dropping it would fail it as
			// RUNNER_LOST a minute later.
			if job := resp.GetJob(); job != nil {
				a.RunJob(context.WithoutCancel(ctx), job)
			}
		case ctx.Err() != nil:
			return nil
		case status.Code(err) == codes.Unauthenticated:
			return ErrUnauthenticated
		default:
			a.Log.Warn("ask ci-svc for a job", slog.String("err", err.Error()), slog.Duration("retry_in", backoff))
			sleep(ctx, backoff)
			backoff = min(backoff*2, tm.maxBackoff)
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
	tm := a.timing()
	out := newLogShipper(a.Client, job.GetJobId(), secrets, tm)
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
		out.Discard() // ci-svc refuses the job's log too
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
	flushCtx, cancelFlush := context.WithTimeout(ctx, tm.logFlush)
	out.Close(flushCtx)
	cancelFlush()
	a.report(ctx, log, req)
}

// heartbeat extends the job's lease every timings.heartbeat and cancels
// the job when ci-svc says it was canceled or is no longer this runner's.
func (a *Agent) heartbeat(ctx context.Context, jobID string, cancel context.CancelCauseFunc) {
	tm := a.timing()
	t := time.NewTicker(tm.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		hbCtx, done := context.WithTimeout(ctx, tm.call)
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
	tm := a.timing()
	backoff := tm.backoff
	for attempt := 1; ; attempt++ {
		rCtx, cancel := context.WithTimeout(ctx, tm.call)
		_, err := a.Client.UpdateJob(rCtx, req)
		cancel()
		switch {
		case err == nil:
			log.Info("job finished", slog.String("status", req.GetStatus().String()))
			return
		case permanent(err) || attempt == reportAttempts:
			log.Error("report job result", slog.String("err", err.Error()))
			return
		}
		sleep(ctx, backoff)
		backoff = min(backoff*2, tm.maxBackoff)
	}
}

// permanent reports whether ci-svc would refuse a retry of a job's call
// too: the job is finished or not this runner's, the request is wrong, or
// the runner was deleted.
func permanent(err error) bool {
	switch status.Code(err) {
	case codes.NotFound, codes.FailedPrecondition, codes.InvalidArgument, codes.Unauthenticated:
		return true
	}
	return false
}

func ptr[T any](v T) *T { return &v }

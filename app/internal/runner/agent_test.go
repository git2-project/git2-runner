package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
)

// onlyFinal returns the one final report f got, or fails the test.
func onlyFinal(t *testing.T, f *fakeCI) finalCall {
	t.Helper()
	finals := f.finalCalls()
	if len(finals) != 1 {
		t.Fatalf("got %d final reports, want 1", len(finals))
	}
	return finals[0]
}

func exitCode(p *int32) string {
	if p == nil {
		return "unset"
	}
	return fmt.Sprint(*p)
}

func TestRunJobResult(t *testing.T) {
	for _, tc := range []struct {
		name     string
		result   Result
		status   civ1.JobStatus
		reason   civ1.JobFailureReason
		exitCode *int32
		footer   string
	}{
		{"success", Result{}, statusSuccess, reasonNone, ptr(int32(0)), "Job succeeded"},
		{"script failure", Result{ExitCode: 2}, statusFailed, reasonScript, ptr(int32(2)), "Job failed: exit code 2"},
		{"exit code above 255", Result{ExitCode: 300}, statusFailed, reasonScript, ptr(int32(255)), "exit code 300"},
		{"exit code below -1", Result{ExitCode: -9}, statusFailed, reasonScript, ptr(int32(-1)), "exit code -9"},
		{"image pull failure", Result{ExitCode: -1, Failure: reasonPull}, statusFailed, reasonPull, nil, ""},
		{"runner system failure", Result{ExitCode: -1, Failure: reasonSystem}, statusFailed, reasonSystem, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeCI(t)
			var got Spec
			a := newTestAgent(f, execFunc(func(_ context.Context, spec Spec, out io.Writer) Result {
				got = spec
				fmt.Fprintln(out, "hello from the job")
				return tc.result
			}))
			job := testJob(time.Hour,
				&civ1.JobVariable{Name: "CI_JOB_ID", Value: testJobID},
				&civ1.JobVariable{Name: "TOKEN", Value: "hunter22", Masked: true})
			within(t, "RunJob", func() { a.RunJob(context.Background(), job) })

			want := Spec{
				JobID: testJobID, Image: "alpine:3.22", Script: BuildScript([]string{"make"}),
				Env: []string{"CI_JOB_ID=" + testJobID, "TOKEN=hunter22"},
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("executor got %+v, want %+v", got, want)
			}
			final := onlyFinal(t, f)
			req := final.req
			if req.GetJobId() != testJobID || req.GetStatus() != tc.status || req.GetFailureReason() != tc.reason ||
				exitCode(req.ExitCode) != exitCode(tc.exitCode) {
				t.Errorf("reported %v %v exit code %s, want %v %v exit code %s", req.GetStatus(), req.GetFailureReason(),
					exitCode(req.ExitCode), tc.status, tc.reason, exitCode(tc.exitCode))
			}
			if !strings.Contains(final.log, "hello from the job") || !strings.Contains(final.log, tc.footer) {
				t.Errorf("log %q lacks the job's output or %q", final.log, tc.footer)
			}
		})
	}
}

func TestRunJobTimeout(t *testing.T) {
	t.Parallel()
	f := newFakeCI(t)
	var stopped error
	a := newTestAgent(f, execFunc(func(ctx context.Context, _ Spec, _ io.Writer) Result {
		<-ctx.Done()
		stopped = ctx.Err()
		return Result{ExitCode: -1} // what the Docker executor returns when stopped
	}))
	within(t, "RunJob", func() { a.RunJob(context.Background(), testJob(30*time.Millisecond)) })

	if !errors.Is(stopped, context.DeadlineExceeded) {
		t.Errorf("the job's context ended with %v, want the deadline", stopped)
	}
	final := onlyFinal(t, f)
	if req := final.req; req.GetStatus() != statusFailed || req.GetFailureReason() != reasonTimeout || req.ExitCode != nil {
		t.Errorf("reported %v %v exit code %s, want FAILED TIMEOUT without exit code",
			req.GetStatus(), req.GetFailureReason(), exitCode(req.ExitCode))
	}
	if !strings.Contains(final.log, "The job timed out after 30ms.") {
		t.Errorf("log %q does not say the job timed out", final.log)
	}
}

func TestRunJobCanceled(t *testing.T) {
	t.Parallel()
	f := newFakeCI(t)
	f.heartbeat = func(n int) (*civ1.UpdateJobResponse, error) {
		return &civ1.UpdateJobResponse{CancelRequested: n == 2}, nil
	}
	var stopped error
	a := newTestAgent(f, execFunc(func(ctx context.Context, _ Spec, _ io.Writer) Result {
		<-ctx.Done()
		stopped = ctx.Err()
		return Result{ExitCode: -1}
	}))
	within(t, "RunJob", func() { a.RunJob(context.Background(), testJob(time.Hour)) })

	if !errors.Is(stopped, context.Canceled) {
		t.Errorf("the job's context ended with %v, want canceled", stopped)
	}
	if n := f.heartbeats(); n != 2 {
		t.Errorf("%d heartbeats, want 2: none after the cancel", n)
	}
	// ci-svc wants FAILED and records CANCELED; fakeCI checks the rest.
	final := onlyFinal(t, f)
	if final.req.GetStatus() != statusFailed {
		t.Errorf("reported %v, want FAILED", final.req.GetStatus())
	}
	if !strings.Contains(final.log, "The job was canceled.") {
		t.Errorf("log %q does not say the job was canceled", final.log)
	}
}

// TestRunJobLost checks that a job ci-svc no longer gives this runner is
// stopped and not reported, and that the runner does not wait for its log.
func TestRunJobLost(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		heartbeat, appendLog codes.Code
	}{
		{"not this runner's", codes.NotFound, codes.NotFound},
		{"already finished", codes.FailedPrecondition, codes.FailedPrecondition},
		{"runner deleted", codes.Unauthenticated, codes.Unauthenticated},
		{"then ci-svc unreachable", codes.FailedPrecondition, codes.Unavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeCI(t)
			var lost bool // shared by the hooks, under f.mu
			f.heartbeat = func(int) (*civ1.UpdateJobResponse, error) {
				lost = true
				return nil, status.Error(tc.heartbeat, "job gone")
			}
			f.appendFault = func(int) (bool, error) {
				if lost {
					return false, status.Error(tc.appendLog, "job gone")
				}
				return false, nil
			}
			var stopped error
			a := newTestAgent(f, execFunc(func(ctx context.Context, _ Spec, out io.Writer) Result {
				for ctx.Err() == nil {
					fmt.Fprintln(out, "working")
					time.Sleep(time.Millisecond)
				}
				stopped = ctx.Err()
				return Result{ExitCode: -1}
			}))
			within(t, "RunJob", func() { a.RunJob(context.Background(), testJob(time.Hour)) })

			if !errors.Is(stopped, context.Canceled) {
				t.Errorf("the job's context ended with %v, want canceled", stopped)
			}
			if n := f.heartbeats(); n != 1 {
				t.Errorf("%d heartbeats, want 1", n)
			}
			if finals := f.finalCalls(); len(finals) != 0 {
				t.Errorf("reported a job ci-svc took away: %v", finals[0].req)
			}
		})
	}
}

func TestRunJobKeepsRunningWhenHeartbeatsFail(t *testing.T) {
	t.Parallel()
	f := newFakeCI(t)
	f.heartbeat = func(n int) (*civ1.UpdateJobResponse, error) {
		if n%2 == 1 {
			return nil, status.Error(codes.Unavailable, "connection refused")
		}
		return nil, status.Error(codes.DeadlineExceeded, "deadline exceeded")
	}
	var stopped error
	a := newTestAgent(f, execFunc(func(ctx context.Context, _ Spec, _ io.Writer) Result {
		for f.heartbeats() < 4 && ctx.Err() == nil {
			time.Sleep(time.Millisecond)
		}
		stopped = ctx.Err()
		return Result{}
	}))
	within(t, "RunJob", func() { a.RunJob(context.Background(), testJob(time.Hour)) })

	if stopped != nil {
		t.Errorf("the job was stopped (%v) because heartbeats failed", stopped)
	}
	if final := onlyFinal(t, f); final.req.GetStatus() != statusSuccess {
		t.Errorf("reported %v, want SUCCESS", final.req.GetStatus())
	}
}

func TestReportRetries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures int // final reports refused before one is accepted
		code     codes.Code
		attempts int
	}{
		{"unavailable, then accepted", 2, codes.Unavailable, 3},
		{"deadline exceeded, then accepted", 1, codes.DeadlineExceeded, 2},
		{"unavailable every time", 100, codes.Unavailable, reportAttempts},
		{"not this runner's job", 1, codes.NotFound, 1},
		{"already finished", 1, codes.FailedPrecondition, 1},
		{"invalid report", 1, codes.InvalidArgument, 1},
		{"runner deleted", 1, codes.Unauthenticated, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeCI(t)
			f.final = func(n int) error {
				if n <= tc.failures {
					return status.Error(tc.code, "refused")
				}
				return nil
			}
			a := newTestAgent(f, execFunc(func(context.Context, Spec, io.Writer) Result { return Result{} }))
			within(t, "RunJob", func() { a.RunJob(context.Background(), testJob(time.Hour)) })

			if n := len(f.finalCalls()); n != tc.attempts {
				t.Errorf("sent the final report %d times, want %d", n, tc.attempts)
			}
		})
	}
}

func TestRunUnauthenticated(t *testing.T) {
	t.Parallel()
	f := newFakeCI(t)
	f.requestJob = func(context.Context, int) (*civ1.RequestJobResponse, error) {
		return nil, status.Error(codes.Unauthenticated, "unknown runner token")
	}
	a := newTestAgent(f, nil)
	var err error
	within(t, "Run", func() { err = a.Run(context.Background()) })

	if !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("Run returned %v, want ErrUnauthenticated", err)
	}
	if n := f.requestCalls(); n != 1 {
		t.Errorf("%d RequestJob calls, want 1", n)
	}
}

func TestRunBacksOff(t *testing.T) {
	t.Parallel()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	f := newFakeCI(t)
	var calls []time.Time // written by Run's goroutine, read after Run returned
	f.requestJob = func(_ context.Context, n int) (*civ1.RequestJobResponse, error) {
		calls = append(calls, time.Now())
		switch n {
		case 3:
			return nil, errors.New("connection reset") // not a gRPC status
		case 6:
			stop()
			return nil, status.Error(codes.Canceled, "context canceled")
		}
		return nil, status.Error(codes.Unavailable, "connection refused")
	}
	a := newTestAgent(f, nil)
	a.timings.backoff, a.timings.maxBackoff = 10*time.Millisecond, 40*time.Millisecond
	var err error
	within(t, "Run", func() { err = a.Run(ctx) })

	if err != nil {
		t.Errorf("Run returned %v, want nil after the shutdown", err)
	}
	if len(calls) != 6 {
		t.Fatalf("%d RequestJob calls, want 6", len(calls))
	}
	for i, want := range []time.Duration{10, 20, 40, 40, 40} {
		if gap := calls[i+1].Sub(calls[i]); gap < want*time.Millisecond {
			t.Errorf("waited %s before RequestJob %d, want at least %s", gap, i+2, want*time.Millisecond)
		}
	}
}

// TestRunFinishesJobOnShutdown checks that a SIGTERM during a job lets the
// job finish and be reported, and that no new job is asked for.
func TestRunFinishesJobOnShutdown(t *testing.T) {
	t.Parallel()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	f := newFakeCI(t)
	f.requestJob = func(ctx context.Context, n int) (*civ1.RequestJobResponse, error) {
		if n == 1 {
			return &civ1.RequestJobResponse{Job: testJob(time.Hour)}, nil
		}
		t.Errorf("RequestJob %d after the shutdown", n)
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	var stopped error
	a := newTestAgent(f, execFunc(func(ctx context.Context, _ Spec, _ io.Writer) Result {
		stop() // the SIGTERM
		stopped = ctx.Err()
		return Result{}
	}))
	var err error
	within(t, "Run", func() { err = a.Run(ctx) })

	if err != nil {
		t.Errorf("Run returned %v, want nil", err)
	}
	if stopped != nil {
		t.Errorf("the SIGTERM stopped the running job: %v", stopped)
	}
	final := onlyFinal(t, f)
	if final.req.GetStatus() != statusSuccess || !strings.Contains(final.log, "Job succeeded") {
		t.Errorf("reported %v with log %q, want SUCCESS with the whole log", final.req.GetStatus(), final.log)
	}
}

// TestRunRunsJobArrivingWithShutdown checks that a job whose RequestJob
// reply arrives together with the SIGTERM is run, not dropped: ci-svc has
// already leased it to this runner.
func TestRunRunsJobArrivingWithShutdown(t *testing.T) {
	t.Parallel()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	f := newFakeCI(t)
	f.requestJob = func(ctx context.Context, n int) (*civ1.RequestJobResponse, error) {
		if n == 1 {
			stop() // the SIGTERM comes while the reply is on its way
			return &civ1.RequestJobResponse{Job: testJob(time.Hour)}, nil
		}
		t.Errorf("RequestJob %d after the shutdown", n)
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	ran := false
	a := newTestAgent(f, execFunc(func(context.Context, Spec, io.Writer) Result {
		ran = true
		return Result{}
	}))
	var err error
	within(t, "Run", func() { err = a.Run(ctx) })

	if err != nil {
		t.Errorf("Run returned %v, want nil", err)
	}
	if !ran {
		t.Fatal("the job was dropped")
	}
	if final := onlyFinal(t, f); final.req.GetStatus() != statusSuccess {
		t.Errorf("reported %v, want SUCCESS", final.req.GetStatus())
	}
}

package runner

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
)

// testTimings make the agent fast enough for unit tests.
var testTimings = timings{
	request:    time.Second,
	heartbeat:  5 * time.Millisecond,
	call:       time.Second,
	backoff:    time.Millisecond,
	maxBackoff: 4 * time.Millisecond,
	logFlush:   time.Second,
	flushEvery: 2 * time.Millisecond,
}

const testJobID = "0b6f1f9e-4a1c-4c55-9d6e-2f1f3b8a9c01"

// fakeTruncatedNotice ends a log that reached fakeCI.maxLog.
const fakeTruncatedNotice = "\n[truncated]\n"

// Short names for the statuses and reasons the tests compare.
const (
	statusSuccess = civ1.JobStatus_JOB_STATUS_SUCCESS
	statusFailed  = civ1.JobStatus_JOB_STATUS_FAILED
	reasonNone    = civ1.JobFailureReason_JOB_FAILURE_REASON_UNSPECIFIED
	reasonScript  = civ1.JobFailureReason_JOB_FAILURE_REASON_SCRIPT_FAILURE
	reasonTimeout = civ1.JobFailureReason_JOB_FAILURE_REASON_TIMEOUT
	reasonLost    = civ1.JobFailureReason_JOB_FAILURE_REASON_RUNNER_LOST
	reasonPull    = civ1.JobFailureReason_JOB_FAILURE_REASON_IMAGE_PULL_FAILURE
	reasonSystem  = civ1.JobFailureReason_JOB_FAILURE_REASON_RUNNER_SYSTEM_FAILURE
)

// fakeCI stands in for ci-svc's runner port. Like a real client, a call
// with an ended context fails before it reaches the server. AppendLog
// stores the log as ci-svc does: bytes before the end of the log are
// ignored, and the log stops at maxLog. A final UpdateJob that ci-svc
// would refuse fails the test.
//
// The hooks change the answers. All but requestJob run with mu held, so
// they may share variables with each other but must not call f's methods.
type fakeCI struct {
	t *testing.T

	// requestJob answers RequestJob call n (from 1). Unset, it waits like
	// a long poll that finds no job.
	requestJob func(ctx context.Context, n int) (*civ1.RequestJobResponse, error)
	// heartbeat answers heartbeat n (from 1). Unset, it says carry on.
	heartbeat func(n int) (*civ1.UpdateJobResponse, error)
	// final answers final report n (from 1). Unset, it accepts it.
	final func(n int) error
	// appendFault makes AppendLog call n (from 1) fail with err. With
	// keep, ci-svc stores the bytes first, as when only the reply is lost.
	appendFault func(n int) (keep bool, err error)
	// maxLog is the size at which the log is truncated; 0 means never.
	maxLog int

	mu        sync.Mutex
	requests  int
	beats     int
	finals    []finalCall
	appends   []appendCall
	log       []byte
	truncated bool
}

// finalCall is a final UpdateJob, with the log ci-svc held when it came.
type finalCall struct {
	req *civ1.UpdateJobRequest
	log string
}

// appendCall is an AppendLog.
type appendCall struct {
	offset     int64
	data       []byte
	ok         bool
	size       int64 // the size ci-svc answered, if ok
	afterFinal bool  // sent after a final UpdateJob
}

func newFakeCI(t *testing.T) *fakeCI { return &fakeCI{t: t} }

func (f *fakeCI) RegisterRunner(context.Context, *civ1.RegisterRunnerRequest, ...grpc.CallOption) (*civ1.RegisterRunnerResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not in the fake")
}

func (f *fakeCI) RequestJob(ctx context.Context, _ *civ1.RequestJobRequest, _ ...grpc.CallOption) (*civ1.RequestJobResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	f.mu.Lock()
	f.requests++
	n := f.requests
	f.mu.Unlock()
	if f.requestJob != nil {
		return f.requestJob(ctx, n)
	}
	<-ctx.Done()
	return nil, status.FromContextError(ctx.Err()).Err()
}

func (f *fakeCI) UpdateJob(ctx context.Context, in *civ1.UpdateJobRequest, _ ...grpc.CallOption) (*civ1.UpdateJobResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if in.GetStatus() == civ1.JobStatus_JOB_STATUS_RUNNING {
		f.beats++
		if f.heartbeat != nil {
			return f.heartbeat(f.beats)
		}
		return &civ1.UpdateJobResponse{}, nil
	}
	f.finals = append(f.finals, finalCall{req: proto.CloneOf(in), log: string(f.log)})
	if err := checkFinal(in); err != nil {
		f.t.Errorf("ci-svc would refuse the final report {%v}: %v", in, err)
		return nil, err
	}
	if f.final != nil {
		if err := f.final(len(f.finals)); err != nil {
			return nil, err
		}
	}
	return &civ1.UpdateJobResponse{}, nil
}

func (f *fakeCI) AppendLog(ctx context.Context, in *civ1.AppendLogRequest, _ ...grpc.CallOption) (*civ1.AppendLogResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appends = append(f.appends, appendCall{offset: in.GetOffset(), data: bytes.Clone(in.GetData()), afterFinal: len(f.finals) > 0})
	call := &f.appends[len(f.appends)-1]
	if in.GetOffset() < 0 || len(in.GetData()) == 0 || len(in.GetData()) > 256<<10 {
		f.t.Errorf("ci-svc would refuse AppendLog with %d bytes at offset %d", len(in.GetData()), in.GetOffset())
		return nil, status.Error(codes.InvalidArgument, "bad offset or data")
	}
	var keep bool
	var err error
	if f.appendFault != nil {
		keep, err = f.appendFault(len(f.appends))
	}
	if err == nil || keep {
		f.store(in.GetOffset(), in.GetData())
	}
	if err != nil {
		return nil, err
	}
	call.ok, call.size = true, int64(len(f.log))
	return &civ1.AppendLogResponse{Size: call.size, Truncated: f.truncated}, nil
}

// store appends data at offset the way ci-svc does; f.mu is held.
func (f *fakeCI) store(offset int64, data []byte) {
	if f.maxLog > 0 && len(f.log) >= f.maxLog {
		f.truncated = true
	}
	skip := int64(len(f.log)) - offset
	if f.truncated || skip < 0 || skip >= int64(len(data)) {
		return
	}
	chunk := data[skip:]
	if f.maxLog > 0 && len(f.log)+len(chunk) > f.maxLog {
		room := f.maxLog - len(f.log)
		chunk = append(chunk[:room:room], fakeTruncatedNotice...)
		f.truncated = true
	}
	f.log = append(f.log, chunk...)
}

// checkFinal refuses what ci-svc's UpdateJob refuses in a final report.
func checkFinal(in *civ1.UpdateJobRequest) error {
	reason := in.GetFailureReason()
	switch in.GetStatus() {
	case statusSuccess:
		if reason != reasonNone || in.GetExitCode() != 0 {
			return status.Error(codes.InvalidArgument, "SUCCESS takes exit code 0 and no failure_reason")
		}
	case statusFailed:
		switch {
		case reason == reasonNone || reason == reasonLost:
			return status.Error(codes.InvalidArgument, "FAILED needs a failure_reason other than RUNNER_LOST")
		case reason == reasonScript && in.ExitCode == nil:
			return status.Error(codes.InvalidArgument, "SCRIPT_FAILURE needs an exit_code")
		}
	default:
		return status.Error(codes.InvalidArgument, "status must be RUNNING, SUCCESS or FAILED")
	}
	return nil
}

func (f *fakeCI) requestCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *fakeCI) heartbeats() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.beats
}

func (f *fakeCI) finalCalls() []finalCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.finals)
}

func (f *fakeCI) appendCalls() []appendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.appends)
}

func (f *fakeCI) storedLog() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.log)
}

// checkOffsets fails the test unless every AppendLog started at the
// offset ci-svc confirmed last.
func checkOffsets(t *testing.T, calls []appendCall) {
	t.Helper()
	var confirmed int64
	for i, c := range calls {
		if c.offset != confirmed {
			t.Errorf("AppendLog %d at offset %d, want the confirmed offset %d", i+1, c.offset, confirmed)
		}
		if c.ok {
			confirmed = c.size
		}
	}
}

// execFunc is an Executor made of a function.
type execFunc func(ctx context.Context, spec Spec, out io.Writer) Result

func (e execFunc) Run(ctx context.Context, spec Spec, out io.Writer) Result { return e(ctx, spec, out) }

// newTestAgent returns an agent with testTimings and no log output.
func newTestAgent(f *fakeCI, exec Executor) *Agent {
	tm := testTimings
	return &Agent{Client: f, Executor: exec, Log: slog.New(slog.DiscardHandler), timings: &tm}
}

// testJob is a job as ci-svc assigns it.
func testJob(timeout time.Duration, vars ...*civ1.JobVariable) *civ1.JobAssignment {
	return &civ1.JobAssignment{
		JobId: testJobID, Name: "build", Image: "alpine:3.22",
		Script: []string{"make"}, Variables: vars, Timeout: durationpb.New(timeout),
	}
}

// within fails the test if fn has not returned after 5 seconds, so a hang
// shows up as a failure instead of a stuck test run.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return", what)
	}
}

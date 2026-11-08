package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
)

// newTestShipper returns a shipper whose timer never fires: it sends only
// when the test calls flush or Close, or when its buffer fills.
func newTestShipper(f *fakeCI, secrets ...string) *logShipper {
	tm := testTimings
	tm.flushEvery = time.Hour
	return newLogShipper(f, testJobID, secrets, &tm)
}

func TestLogShipperResendsAtConfirmedOffset(t *testing.T) {
	for _, tc := range []struct {
		name string
		keep bool
		err  error
	}{
		{"lost before ci-svc stored it", false, status.Error(codes.Unavailable, "connection reset")},
		{"stored, but the reply was lost", true, status.Error(codes.DeadlineExceeded, "deadline exceeded")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeCI(t)
			f.appendFault = func(n int) (bool, error) {
				if n == 2 {
					return tc.keep, tc.err
				}
				return false, nil
			}
			s := newTestShipper(f)
			ctx := context.Background()
			_, _ = io.WriteString(s, "one\n")
			s.flush(ctx)
			_, _ = io.WriteString(s, "two\n")
			s.flush(ctx) // fails
			_, _ = io.WriteString(s, "three\n")
			within(t, "Close", func() { s.Close(ctx) })

			if got := f.storedLog(); got != "one\ntwo\nthree\n" {
				t.Errorf("stored log %q, want every line once", got)
			}
			calls := f.appendCalls()
			checkOffsets(t, calls)
			if len(calls) != 3 {
				t.Errorf("%d AppendLog calls, want 3", len(calls))
			}
		})
	}
}

func TestLogShipperSplitsLargeOutput(t *testing.T) {
	t.Parallel()
	f := newFakeCI(t)
	s := newTestShipper(f)
	out := bytes.Repeat([]byte("0123456789abcdef"), 3*maxSend/16+5)
	_, _ = s.Write(out)
	within(t, "Close", func() { s.Close(context.Background()) })

	if got := f.storedLog(); got != string(out) {
		t.Errorf("stored %d bytes, want the %d written", len(got), len(out))
	}
	calls := f.appendCalls()
	checkOffsets(t, calls)
	for i, c := range calls {
		if len(c.data) > maxSend {
			t.Errorf("AppendLog %d carried %d bytes, more than %d", i+1, len(c.data), maxSend)
		}
	}
}

// TestLogShipperDropsOldest checks that output past the buffer limit
// loses its oldest bytes, and that the rest is stored without a gap.
func TestLogShipperDropsOldest(t *testing.T) {
	t.Parallel()
	f := newFakeCI(t)
	s := newTestShipper(f)
	out := make([]byte, maxBuffered+100)
	for i := range out {
		out[i] = 'a' + byte(i%26)
	}
	_, _ = s.Write(out)
	within(t, "Close", func() { s.Close(context.Background()) })

	if got := f.storedLog(); got != string(out[100:]) {
		t.Errorf("stored %d bytes, want the last %d written", len(got), maxBuffered)
	}
	checkOffsets(t, f.appendCalls())
}

func TestLogShipperStopsWhenTruncated(t *testing.T) {
	t.Parallel()
	f := newFakeCI(t)
	f.maxLog = 10
	s := newTestShipper(f)
	ctx := context.Background()
	_, _ = io.WriteString(s, "123456")
	s.flush(ctx)
	_, _ = io.WriteString(s, "7890ABCD")
	// A shipper that misses the truncation resends forever: the log no
	// longer grows.
	within(t, "flush", func() { s.flush(ctx) })
	_, _ = io.WriteString(s, "after the limit\n")
	within(t, "flush", func() { s.flush(ctx) })
	within(t, "Close", func() { s.Close(ctx) })

	if n := len(f.appendCalls()); n != 2 {
		t.Errorf("%d AppendLog calls, want 2: none after the log was truncated", n)
	}
	if got, want := f.storedLog(), "1234567890"+fakeTruncatedNotice; got != want {
		t.Errorf("stored log %q, want %q", got, want)
	}
}

// TestLogShipperStopsWhenRefused checks that the shipper stops sending
// once ci-svc refuses the log for good, instead of retrying forever.
func TestLogShipperStopsWhenRefused(t *testing.T) {
	for _, code := range []codes.Code{codes.NotFound, codes.FailedPrecondition, codes.InvalidArgument, codes.Unauthenticated} {
		t.Run(code.String(), func(t *testing.T) {
			t.Parallel()
			f := newFakeCI(t)
			f.appendFault = func(n int) (bool, error) {
				if n >= 2 {
					return false, status.Error(code, "refused")
				}
				return false, nil
			}
			s := newTestShipper(f)
			ctx := context.Background()
			_, _ = io.WriteString(s, "one\n")
			s.flush(ctx)
			_, _ = io.WriteString(s, "two\n")
			s.flush(ctx) // refused
			_, _ = io.WriteString(s, "three\n")
			within(t, "Close", func() { s.Close(ctx) })

			if n := len(f.appendCalls()); n != 2 {
				t.Errorf("%d AppendLog calls, want 2: none after ci-svc refused the log", n)
			}
		})
	}
}

func TestLogShipperCloseGivesUp(t *testing.T) {
	t.Parallel()
	f := newFakeCI(t)
	f.appendFault = func(int) (bool, error) { return false, status.Error(codes.Unavailable, "connection refused") }
	s := newTestShipper(f)
	_, _ = io.WriteString(s, "never stored\n")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	within(t, "Close", func() { s.Close(ctx) })

	if len(f.appendCalls()) < 2 {
		t.Error("Close did not retry while it had time")
	}
}

// TestRunJobShipsWholeMaskedLog runs a job that prints its secrets and
// checks that ci-svc never sees them, and that the whole log reaches
// ci-svc, once, before the final status.
func TestRunJobShipsWholeMaskedLog(t *testing.T) {
	t.Parallel()
	const token = "hunter22-very-secret" //nolint:gosec // a made-up secret for the test
	const key = "-----BEGIN KEY-----\nAAAABBBBCCCC\nDDDDEEEEFFFF\n-----END KEY-----"
	f := newFakeCI(t)
	f.appendFault = func(n int) (bool, error) {
		if n == 2 {
			return true, status.Error(codes.Unavailable, "reply lost")
		}
		return false, nil
	}
	a := newTestAgent(f, execFunc(func(_ context.Context, _ Spec, out io.Writer) Result {
		fmt.Fprintf(out, "token: %s\n", token)
		_, _ = io.WriteString(out, "split: hunter22-")
		time.Sleep(10 * time.Millisecond) // the shipper sends what it has meanwhile
		_, _ = io.WriteString(out, "very-secret\n")
		fmt.Fprintf(out, "key:\n%s\none line of it: DDDDEEEEFFFF\n", key)
		_, _ = io.WriteString(out, "plain: visible-value\n")
		for i := range 5000 {
			fmt.Fprintf(out, "line %d\n", i)
		}
		_, _ = io.WriteString(out, "last line\n")
		return Result{}
	}))
	job := testJob(time.Hour,
		&civ1.JobVariable{Name: "TOKEN", Value: token, Masked: true},
		&civ1.JobVariable{Name: "KEY", Value: key, Masked: true},
		&civ1.JobVariable{Name: "PLAIN", Value: "visible-value"})
	within(t, "RunJob", func() { a.RunJob(context.Background(), job) })

	log, calls := f.storedLog(), f.appendCalls()
	for _, secret := range []string{token, "hunter22", "AAAABBBBCCCC", "DDDDEEEEFFFF", "-----BEGIN KEY-----"} {
		if strings.Contains(log, secret) {
			t.Errorf("the stored log contains %q", secret)
		}
		for i, c := range calls {
			if bytes.Contains(c.data, []byte(secret)) {
				t.Errorf("AppendLog %d carried %q", i+1, secret)
			}
		}
	}
	if n := strings.Count(log, Masked); n != 4 {
		t.Errorf("%d %s in the log, want 4", n, Masked)
	}
	if !strings.Contains(log, "plain: visible-value\n") || !strings.Contains(log, "line 4999\nlast line\n") {
		t.Error("the log lacks output that is not secret")
	}

	final := onlyFinal(t, f)
	if final.log != log {
		t.Errorf("%d bytes of log reached ci-svc after the final status", len(log)-len(final.log))
	}
	if !strings.HasSuffix(final.log, "Job succeeded\x1b[0m\n") {
		t.Errorf("the log before the final status ends with %q", final.log[max(0, len(final.log)-40):])
	}
	for i, c := range calls {
		if c.afterFinal {
			t.Errorf("AppendLog %d came after the final status", i+1)
		}
	}
	checkOffsets(t, calls)
}

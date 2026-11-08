package runner

import (
	"context"
	"sync"
	"time"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
)

// Log shipping limits.
const (
	// maxSend is the most bytes one AppendLog carries; ci-svc accepts 256 KiB.
	maxSend = 128 << 10
	// maxBuffered is how much output waits for ci-svc before the oldest is
	// dropped. A job printing faster than ci-svc accepts loses log, never
	// time.
	maxBuffered = 8 << 20
)

// logShipper is the io.Writer a job's output goes to. It masks secrets,
// then sends the bytes to ci-svc with AppendLog every timings.flushEvery,
// from the offset ci-svc confirmed last. A failed send is retried on the
// next flush; ci-svc ignores bytes it already has.
type logShipper struct {
	client civ1.RunnerServiceClient
	jobID  string
	tm     *timings

	mu      sync.Mutex
	mask    *masker
	buf     []byte // masked, not yet confirmed by ci-svc
	offset  int64  // log size ci-svc confirmed
	stopped bool   // ci-svc truncated or refused the log: send nothing more

	kick chan struct{}
	done chan struct{}
	stop context.CancelFunc
}

func newLogShipper(client civ1.RunnerServiceClient, jobID string, secrets []string, tm *timings) *logShipper {
	ctx, cancel := context.WithCancel(context.Background())
	s := &logShipper{
		client: client, jobID: jobID, tm: tm, mask: newMasker(secrets),
		kick: make(chan struct{}, 1), done: make(chan struct{}), stop: cancel,
	}
	go s.loop(ctx)
	return s
}

// Write masks p and queues it. It never blocks on the network.
func (s *logShipper) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.append(s.mask.write(p))
	full := len(s.buf) >= maxSend
	s.mu.Unlock()
	if full {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

// append adds masked bytes; the caller holds mu.
func (s *logShipper) append(b []byte) {
	if s.stopped {
		return
	}
	s.buf = append(s.buf, b...)
	if over := len(s.buf) - maxBuffered; over > 0 {
		// The oldest output is lost; the rest is sent at the offset ci-svc
		// confirmed, so the stored log has no gap, only missing lines.
		s.buf = s.buf[over:]
	}
}

func (s *logShipper) loop(ctx context.Context) {
	defer close(s.done)
	t := time.NewTicker(s.tm.flushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
		s.flush(ctx)
	}
}

// flush sends what is buffered, in pieces of at most maxSend bytes. It
// stops at the first error and leaves the rest for the next flush. It
// reports whether nothing is left to send.
func (s *logShipper) flush(ctx context.Context) bool {
	for {
		s.mu.Lock()
		if len(s.buf) == 0 || s.stopped {
			s.mu.Unlock()
			return true
		}
		n := min(len(s.buf), maxSend)
		chunk := append([]byte(nil), s.buf[:n]...)
		offset := s.offset
		s.mu.Unlock()

		sendCtx, cancel := context.WithTimeout(ctx, s.tm.call)
		resp, err := s.client.AppendLog(sendCtx, &civ1.AppendLogRequest{JobId: s.jobID, Offset: offset, Data: chunk})
		cancel()
		if permanent(err) {
			// The job is finished or no longer this runner's: every retry
			// would be refused too.
			s.mu.Lock()
			s.stopped, s.buf = true, nil
			s.mu.Unlock()
			return true
		}
		if err != nil {
			return false
		}

		s.mu.Lock()
		// ci-svc's size says where its log ends. Normally that is
		// offset+n; after dropped output it can be lower, and then we
		// continue from where ci-svc is.
		if size := resp.GetSize(); size > s.offset {
			consumed := min(int(size-s.offset), len(s.buf))
			s.buf = s.buf[consumed:]
			s.offset = size
		} else if size < s.offset {
			s.offset = size
		}
		if resp.GetTruncated() {
			s.stopped = true
			s.buf = nil
		}
		s.mu.Unlock()
	}
}

// Close sends everything still buffered, trying until ctx ends.
func (s *logShipper) Close(ctx context.Context) {
	s.stop()
	<-s.done
	s.mu.Lock()
	s.append(s.mask.close())
	s.mu.Unlock()
	for ctx.Err() == nil {
		if s.flush(ctx) {
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(s.tm.backoff):
		}
	}
}

// Discard stops sending and drops what is still buffered, for a job whose
// log ci-svc no longer takes.
func (s *logShipper) Discard() {
	s.stop()
	<-s.done
}

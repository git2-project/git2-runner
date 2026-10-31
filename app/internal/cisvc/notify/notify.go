// Package notify carries ci-svc's wake-up signals: "a job was queued" and
// "a job's log grew". It is how a long-polling runner hears about a new
// job, and how a log follower hears about new output, within milliseconds.
//
// A signal carries no data. Whoever wakes up reads the database, which is
// the source of truth (ADR 0003). Signals reach waiters in the same
// process directly, and waiters in other ci-svc replicas through the
// Redis shared by all git2 services, on channels starting with "ci:".
//
// Signals can be lost: Redis may be down, or a message may be dropped
// during a reconnect. Every waiter therefore also polls the database on a
// timer, and a Redis fault never makes ci-svc unready.
package notify

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Topics, which are also the Redis channel names.
const (
	// JobsTopic is published when jobs are queued.
	JobsTopic = "ci:jobs:wake"
	// logTopicPrefix plus a job ID is published when the job's log grows
	// or the job finishes.
	logTopicPrefix = "ci:log:"
)

// LogTopic is the topic of one job's log.
func LogTopic(jobID uuid.UUID) string { return logTopicPrefix + jobID.String() }

// publishTimeout bounds one PUBLISH, so a stalled Redis cannot slow down a
// request that already committed.
const publishTimeout = 500 * time.Millisecond

// warnEvery limits how often a failing Redis is logged.
const warnEvery = time.Minute

// RedisOptions are the client settings every ci-svc process uses for the
// shared Redis. The timeouts are short, as in git2's services: a stalled
// Redis must fail fast, because ci-svc falls back to polling.
func RedisOptions(addr, password string) *redis.Options {
	return &redis.Options{
		Addr:         addr,
		Password:     password,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  500 * time.Millisecond,
		WriteTimeout: 500 * time.Millisecond,
	}
}

// Hub delivers signals to waiters in this process and, when it has a
// Redis client, to waiters in other replicas.
type Hub struct {
	rdb    *redis.Client // nil: this process only
	origin string        // marks our own messages, so we skip their echo
	log    *slog.Logger

	mu       sync.Mutex
	subs     map[string]map[chan struct{}]struct{}
	lastWarn time.Time
}

// New returns a Hub. rdb may be nil, for a single replica without Redis.
func New(rdb *redis.Client, log *slog.Logger) *Hub {
	return &Hub{
		rdb:    rdb,
		origin: uuid.NewString(),
		log:    log,
		subs:   map[string]map[chan struct{}]struct{}{},
	}
}

// Subscribe returns a channel that receives a value when topic is
// published, and a function that unsubscribes. Signals that arrive while
// the channel is full are merged: one pending value means "something
// happened since you last looked".
func (h *Hub) Subscribe(topic string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs[topic] == nil {
		h.subs[topic] = map[chan struct{}]struct{}{}
	}
	h.subs[topic][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs[topic], ch)
		if len(h.subs[topic]) == 0 {
			delete(h.subs, topic)
		}
		h.mu.Unlock()
	}
}

// Publish wakes the waiters of topic in this process and, through Redis,
// in the other replicas. Call it after the transaction that made the
// change has committed. It never fails: a lost signal only costs latency.
func (h *Hub) Publish(ctx context.Context, topic string) {
	h.deliver(topic)
	if h.rdb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
	defer cancel()
	if err := h.rdb.Publish(ctx, topic, h.origin).Err(); err != nil {
		h.warn("publish to redis failed; other replicas fall back to polling", err)
	}
}

// deliver wakes the local waiters of topic.
func (h *Hub) deliver(topic string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[topic] {
		select {
		case ch <- struct{}{}:
		default: // already has a pending signal
		}
	}
}

// Run receives signals from other replicas through Redis until ctx ends.
// Without a Redis client it just waits for ctx. go-redis reconnects and
// resubscribes on its own after a Redis restart.
func (h *Hub) Run(ctx context.Context) {
	if h.rdb == nil {
		<-ctx.Done()
		return
	}
	ps := h.rdb.PSubscribe(ctx, JobsTopic, logTopicPrefix+"*")
	defer func() { _ = ps.Close() }()
	msgs := ps.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-msgs:
			if !ok {
				return
			}
			if msg.Payload != h.origin {
				h.deliver(msg.Channel)
			}
		}
	}
}

func (h *Hub) warn(msg string, err error) {
	h.mu.Lock()
	now := time.Now()
	if now.Sub(h.lastWarn) < warnEvery {
		h.mu.Unlock()
		return
	}
	h.lastWarn = now
	h.mu.Unlock()
	h.log.Warn(msg, slog.String("err", err.Error()))
}

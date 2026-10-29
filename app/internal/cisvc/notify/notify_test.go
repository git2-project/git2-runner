package notify

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func received(ch <-chan struct{}, within time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(within):
		return false
	}
}

func TestLocal(t *testing.T) {
	h := New(nil, quiet)
	jobs, unsubscribe := h.Subscribe(JobsTopic)
	logs, _ := h.Subscribe(LogTopic(uuid.New()))

	h.Publish(t.Context(), JobsTopic)
	h.Publish(t.Context(), JobsTopic) // merged with the first
	if !received(jobs, time.Second) {
		t.Fatal("no signal")
	}
	if received(jobs, 50*time.Millisecond) {
		t.Error("two signals while not looking must merge into one")
	}
	if received(logs, 50*time.Millisecond) {
		t.Error("a signal reached another topic")
	}
	unsubscribe()
	h.Publish(t.Context(), JobsTopic)
	if received(jobs, 50*time.Millisecond) {
		t.Error("signal after unsubscribe")
	}
}

func TestAcrossReplicas(t *testing.T) {
	mr := miniredis.RunT(t)
	newHub := func() *Hub {
		rdb := redis.NewClient(RedisOptions(mr.Addr(), ""))
		t.Cleanup(func() { _ = rdb.Close() })
		h := New(rdb, quiet)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go h.Run(ctx)
		return h
	}
	a, b := newHub(), newHub()
	job := uuid.New()
	onA, _ := a.Subscribe(LogTopic(job))
	onB, _ := b.Subscribe(LogTopic(job))

	// Wait until both hubs are subscribed, and longer than the client's
	// read timeout, which must not end the subscription.
	deadline := time.Now().Add(2 * time.Second)
	for mr.PubSubNumPat() < 4 { // 2 hubs x 2 patterns
		if time.Now().After(deadline) {
			t.Fatal("hubs did not subscribe")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(700 * time.Millisecond)

	a.Publish(t.Context(), LogTopic(job))
	if !received(onB, 2*time.Second) {
		t.Fatal("the other replica did not get the signal")
	}
	if !received(onA, time.Second) {
		t.Fatal("the publishing replica did not get its own signal")
	}
	if received(onA, 200*time.Millisecond) {
		t.Error("the publishing replica got the Redis echo of its own signal")
	}
}

func TestRedisDown(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(RedisOptions(mr.Addr(), ""))
	t.Cleanup(func() { _ = rdb.Close() })
	h := New(rdb, quiet)
	mr.Close()

	ch, _ := h.Subscribe(JobsTopic)
	start := time.Now()
	h.Publish(t.Context(), JobsTopic)
	if !received(ch, time.Second) {
		t.Fatal("local waiters must be woken even when Redis is down")
	}
	if time.Since(start) > 3*time.Second {
		t.Error("Publish must not hang on a dead Redis")
	}
}

package ratelimit

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestLimiter(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	l := New(rdb, "register", 2, time.Minute)
	ctx := t.Context()

	for i, want := range []bool{true, true, false} {
		if got := l.Allow(ctx, "10.0.0.1"); got != want {
			t.Fatalf("attempt %d: Allow = %v, want %v", i+1, got, want)
		}
	}
	if !l.Allow(ctx, "10.0.0.2") {
		t.Error("keys are counted separately")
	}
	if !mr.Exists("ci:rl:register:10.0.0.1") || mr.TTL("ci:rl:register:10.0.0.1") <= 0 {
		t.Error("the counter must live under ci:rl: with an expiry")
	}
	mr.FastForward(time.Minute + time.Second)
	if !l.Allow(ctx, "10.0.0.1") {
		t.Error("a new window must start after expiry")
	}

	mr.Close()
	if !l.Allow(ctx, "10.0.0.1") {
		t.Error("a dead Redis must allow (fail open)")
	}
	if !(*Limiter)(nil).Allow(ctx, "x") || !New(nil, "a", 0, time.Second).Allow(ctx, "x") {
		t.Error("no Redis must allow")
	}
}

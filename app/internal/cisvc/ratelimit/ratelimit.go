// Package ratelimit counts attempts per key and time window in the Redis
// shared by all git2 services, under keys "ci:rl:<action>:<key>".
//
// It fails open: when Redis is missing or down, every attempt is allowed.
// A rate limit protects against guessing; it is never what keeps the
// service correct (ADR 0003).
package ratelimit

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// timeout bounds one check, so a stalled Redis does not stall a request.
const timeout = 300 * time.Millisecond

// Limiter allows at most Limit attempts per Window and key.
type Limiter struct {
	rdb    *redis.Client
	action string
	limit  int64
	window time.Duration
}

// New returns a Limiter for one action, such as "register". rdb may be
// nil, which allows everything.
func New(rdb *redis.Client, action string, limit int64, window time.Duration) *Limiter {
	return &Limiter{rdb: rdb, action: action, limit: limit, window: window}
}

// incr counts one attempt and starts the window on the first one, in one
// round trip.
var incr = redis.NewScript(`
local n = redis.call("INCR", KEYS[1])
if n == 1 then redis.call("PEXPIRE", KEYS[1], ARGV[1]) end
return n`)

// Allow counts one attempt for key and reports whether it is within the
// limit.
func (l *Limiter) Allow(ctx context.Context, key string) bool {
	if l == nil || l.rdb == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	n, err := incr.Run(ctx, l.rdb, []string{"ci:rl:" + l.action + ":" + key}, l.window.Milliseconds()).Int64()
	if err != nil {
		return true
	}
	return n <= l.limit
}

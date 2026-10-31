package ratelimit

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// failoverCooldown is how long Redis is left alone after a failure. Without
// it, a Redis that hangs would add redisTimeout to every request of the outage.
const failoverCooldown = 5 * time.Second

// Failover fails open: it uses primary and, when primary fails, answers from
// local. Availability wins over exact accounting for the length of an outage.
type Failover struct {
	log     *slog.Logger
	primary Limiter
	local   Limiter
	now     func() time.Time

	retryAt   atomic.Int64 // unix nanoseconds; primary is skipped until then
	fallbacks atomic.Int64 // requests answered locally since the last warning
}

func NewFailover(log *slog.Logger, primary, local Limiter) *Failover {
	return &Failover{log: log, primary: primary, local: local, now: time.Now}
}

func (f *Failover) Allow(ctx context.Context, key string, limit Limit) (Result, error) {
	now := f.now()
	if now.UnixNano() < f.retryAt.Load() {
		f.fallbacks.Add(1)
		return f.local.Allow(ctx, key, limit)
	}

	res, err := f.primary.Allow(ctx, key, limit)
	if err == nil {
		return res, nil
	}
	// A client that hung up says nothing about Redis.
	if ctx.Err() == nil {
		f.trip(now, err)
	}
	f.fallbacks.Add(1)
	return f.local.Allow(ctx, key, limit)
}

// trip opens the cooldown. Only the request that wins the swap logs, so an
// outage writes one line per cooldown, carrying the count of local answers.
func (f *Failover) trip(now time.Time, err error) {
	old := f.retryAt.Load()
	if now.UnixNano() < old || !f.retryAt.CompareAndSwap(old, now.Add(failoverCooldown).UnixNano()) {
		return
	}
	f.log.Warn("rate limit using local fallback",
		"err", err,
		"fallback_requests", f.fallbacks.Swap(0),
	)
}

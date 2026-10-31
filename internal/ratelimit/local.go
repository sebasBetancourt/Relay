package ratelimit

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	localIdleTTL    = 10 * time.Minute
	maxLocalEntries = 10_000
)

type localEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// Local is a Limiter for one process: the fallback when Redis fails, and the
// only limiter when REDIS_URL is unset. With N replicas the real limit is N
// times the configured one.
type Local struct {
	mu      sync.Mutex
	entries map[string]*localEntry
	now     func() time.Time
}

func NewLocal() *Local {
	return &Local{entries: make(map[string]*localEntry), now: time.Now}
}

func (l *Local) Allow(_ context.Context, key string, limit Limit) (Result, error) {
	now := l.now()
	perSecond := rate.Limit(float64(limit.PerMinute) / 60)

	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.entries[key]
	if !ok {
		l.evict(now)
		e = &localEntry{limiter: rate.NewLimiter(perSecond, limit.Burst)}
		l.entries[key] = e
	}
	e.lastSeen = now
	// A tenant override can change while the entry is alive.
	if e.limiter.Limit() != perSecond {
		e.limiter.SetLimitAt(now, perSecond)
	}
	if e.limiter.Burst() != limit.Burst {
		e.limiter.SetBurstAt(now, limit.Burst)
	}

	if !e.limiter.AllowN(now, 1) {
		return Result{RetryAfter: secondsFor(1-e.limiter.TokensAt(now), perSecond)}, nil
	}
	tokens := e.limiter.TokensAt(now)
	return Result{
		Allowed:    true,
		Remaining:  int(tokens),
		ResetAfter: secondsFor(float64(limit.Burst)-tokens, perSecond),
	}, nil
}

// evict keeps memory bounded, like the tenant cache: sweep idle entries, and
// if still full start over. An idle entry has refilled, so dropping it loses
// nothing; starting over only forgives some spent budget.
func (l *Local) evict(now time.Time) {
	if len(l.entries) < maxLocalEntries {
		return
	}
	for k, e := range l.entries {
		if now.Sub(e.lastSeen) > localIdleTTL {
			delete(l.entries, k)
		}
	}
	if len(l.entries) >= maxLocalEntries {
		clear(l.entries)
	}
}

func secondsFor(tokens float64, perSecond rate.Limit) time.Duration {
	return time.Duration(tokens / float64(perSecond) * float64(time.Second))
}

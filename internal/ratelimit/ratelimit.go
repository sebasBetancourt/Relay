// Package ratelimit decides whether a request may proceed. Redis holds the
// shared budget (GCRA), so the limit is the same on every replica. When Redis
// is missing or failing, an in-memory limiter takes over: the API stays up and
// the limit becomes per replica ([ADR 0009]).
package ratelimit

import (
	"context"
	"time"
)

// Limit is a sustained rate plus the burst allowed on top of it.
type Limit struct {
	PerMinute int
	Burst     int
}

// Result is one decision. RetryAfter is set only when Allowed is false.
// ResetAfter is how long until the budget is full again.
type Result struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
	ResetAfter time.Duration
}

// Limiter spends one request from key's budget.
type Limiter interface {
	Allow(ctx context.Context, key string, limit Limit) (Result, error)
}

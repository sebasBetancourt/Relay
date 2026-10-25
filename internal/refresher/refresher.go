// Package refresher renews access tokens before they expire, so a client
// request does not pay the provider round-trip and a revoked refresh token
// is noticed without waiting for that request.
//
// It does not replace the row lock in store.Accounts.RefreshLocked. Two
// replicas may both notice the same account; SKIP LOCKED keeps a cycle from
// handing it to both, and the lock still collapses a race with a lazy refresh.
// Redis is not used: a cache being down must not stop token renewal.
package refresher

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sebasBetancourt/relay/internal/oauth"
	"github.com/sebasBetancourt/relay/internal/store"
)

const (
	defaultInterval    = time.Minute
	defaultAhead       = 10 * time.Minute
	defaultConcurrency = 8
	defaultBatch       = 50
)

// Claimer selects accounts whose access token expires soon.
// *store.Accounts implements it.
type Claimer interface {
	ClaimExpiring(ctx context.Context, ahead time.Duration, limit int, fn func(ids []string) error) error
}

// AccountRefresher renews one account. *oauth.Service implements it.
type AccountRefresher interface {
	RefreshAccount(ctx context.Context, accountID string, ahead time.Duration) error
}

// Config for Run.
type Config struct {
	Log         *slog.Logger
	Accounts    Claimer
	Refresh     AccountRefresher
	Interval    time.Duration // default 1m
	Ahead       time.Duration // default 10m; tokens expiring sooner are renewed
	Concurrency int           // default 8
	Batch       int           // default 50; one batch per tick, soonest first
}

type stats struct {
	refreshed int
	revoked   int
	failed    int
}

// Run refreshes one batch immediately, then every Interval, until ctx is
// cancelled. A provider or database error is logged and the next tick retries.
// The job never stops the process.
func Run(ctx context.Context, cfg Config) {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	ahead := cfg.Ahead
	if ahead <= 0 {
		ahead = defaultAhead
	}
	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}
	batch := cfg.Batch
	if batch <= 0 {
		batch = defaultBatch
	}

	logCycle(log, cycle(ctx, log, cfg.Accounts, cfg.Refresh, ahead, batch, concurrency))

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logCycle(log, cycle(ctx, log, cfg.Accounts, cfg.Refresh, ahead, batch, concurrency))
		}
	}
}

func logCycle(log *slog.Logger, st stats) {
	if st.refreshed == 0 && st.revoked == 0 && st.failed == 0 {
		return
	}
	log.Info("tokens refreshed",
		"refreshed", st.refreshed,
		"revoked", st.revoked,
		"failed", st.failed,
	)
}

// cycle claims one batch and refreshes it with at most concurrency provider
// calls at once. One account's error does not cancel the rest of the batch.
func cycle(ctx context.Context, log *slog.Logger, claim Claimer, refresh AccountRefresher, ahead time.Duration, batch, concurrency int) stats {
	if ctx.Err() != nil {
		return stats{}
	}
	var ids []string
	err := claim.ClaimExpiring(ctx, ahead, batch, func(claimed []string) error {
		ids = append(ids, claimed...)
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("token refresh claim failed", "err", err)
		}
		return stats{}
	}
	if len(ids) == 0 {
		return stats{}
	}

	const (
		kindRefreshed = iota
		kindRevoked
		kindFailed
	)
	out := make(chan int, len(ids))

	g := new(errgroup.Group)
	g.SetLimit(concurrency)
	for _, id := range ids {
		g.Go(func() error {
			if ctx.Err() != nil {
				return nil
			}
			err := refresh.RefreshAccount(ctx, id, ahead)
			switch {
			case err == nil:
				out <- kindRefreshed
			case errors.Is(err, oauth.ErrAccountRevoked):
				out <- kindRevoked
			case errors.Is(err, oauth.ErrAccountDisconnected), errors.Is(err, store.ErrNotFound):
				// Disconnected or deleted while this cycle held no lock.
			default:
				log.Warn("token refresh failed", "account_id", id, "err", err)
				out <- kindFailed
			}
			return nil // one account must not cancel the batch
		})
	}
	_ = g.Wait()
	close(out)

	var st stats
	for k := range out {
		switch k {
		case kindRefreshed:
			st.refreshed++
		case kindRevoked:
			st.revoked++
		case kindFailed:
			st.failed++
		}
	}
	return st
}

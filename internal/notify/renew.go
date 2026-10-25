package notify

import (
	"context"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"
)

const (
	defaultRenewInterval    = 15 * time.Minute
	defaultRenewAhead       = 24 * time.Hour
	defaultRenewBatch       = 50
	defaultRenewConcurrency = 4
)

// Claimer selects accounts whose subscription should be created or extended.
// *store.Subscriptions implements it.
type Claimer interface {
	ClaimExpiring(ctx context.Context, ahead time.Duration, limit int, fn func(ids []string) error) error
	ClaimMissing(ctx context.Context, limit int, fn func(ids []string) error) error
}

// Maintainer creates and extends subscriptions. *Service implements it.
type Maintainer interface {
	Subscribe(ctx context.Context, accountID string) error
	RenewAccount(ctx context.Context, accountID string) error
}

// Config for Run.
type Config struct {
	Log         *slog.Logger
	Claims      Claimer
	Maintain    Maintainer
	Interval    time.Duration // default 15m
	Ahead       time.Duration // default 24h
	Batch       int           // default 50; one batch per tick
	Concurrency int           // default 4
}

// Run renews one batch of subscriptions that expire within Ahead, and
// subscribes one batch of active accounts that have no row yet. It repeats
// every Interval until ctx is cancelled. A provider 5xx is logged and the
// row is left for the next tick.
func Run(ctx context.Context, cfg Config) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultRenewInterval
	}
	ahead := cfg.Ahead
	if ahead <= 0 {
		ahead = defaultRenewAhead
	}
	batch := cfg.Batch
	if batch <= 0 {
		batch = defaultRenewBatch
	}
	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = defaultRenewConcurrency
	}

	logCycle(log, cycle(ctx, log, cfg.Claims, cfg.Maintain, ahead, batch, concurrency))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logCycle(log, cycle(ctx, log, cfg.Claims, cfg.Maintain, ahead, batch, concurrency))
		}
	}
}

type cycleStats struct {
	renewed    int
	subscribed int
	failed     int
}

func logCycle(log *slog.Logger, st cycleStats) {
	if st.renewed == 0 && st.subscribed == 0 && st.failed == 0 {
		return
	}
	log.Info("calendar subscriptions",
		"renewed", st.renewed,
		"subscribed", st.subscribed,
		"failed", st.failed,
	)
}

func cycle(ctx context.Context, log *slog.Logger, claims Claimer, m Maintainer, ahead time.Duration, batch, concurrency int) cycleStats {
	if ctx.Err() != nil || claims == nil || m == nil {
		return cycleStats{}
	}
	var st cycleStats
	st.failed += runBatch(ctx, log, concurrency, claim(ctx, log, func(fn func([]string) error) error {
		return claims.ClaimExpiring(ctx, ahead, batch, fn)
	}), func(ctx context.Context, id string) error {
		return m.RenewAccount(ctx, id)
	}, &st.renewed)
	st.failed += runBatch(ctx, log, concurrency, claim(ctx, log, func(fn func([]string) error) error {
		return claims.ClaimMissing(ctx, batch, fn)
	}), func(ctx context.Context, id string) error {
		return m.Subscribe(ctx, id)
	}, &st.subscribed)
	return st
}

func claim(ctx context.Context, log *slog.Logger, run func(func([]string) error) error) []string {
	var ids []string
	err := run(func(claimed []string) error {
		ids = append(ids, claimed...)
		return nil
	})
	if err != nil && ctx.Err() == nil {
		log.Warn("subscription claim failed", "err", err)
		return nil
	}
	return ids
}

// runBatch runs fn for each id. One error does not cancel the rest.
// It returns how many failed and adds successes to ok.
func runBatch(ctx context.Context, log *slog.Logger, concurrency int, ids []string, fn func(context.Context, string) error, ok *int) int {
	if len(ids) == 0 {
		return 0
	}
	var failed int
	out := make(chan bool, len(ids))
	g := new(errgroup.Group)
	g.SetLimit(concurrency)
	for _, id := range ids {
		g.Go(func() error {
			if ctx.Err() != nil {
				return nil
			}
			err := fn(ctx, id)
			if err != nil {
				log.Warn("subscription maintain failed", "account_id", id, "err", err)
				out <- false
				return nil
			}
			out <- true
			return nil
		})
	}
	_ = g.Wait()
	close(out)
	for success := range out {
		if success {
			*ok++
		} else {
			failed++
		}
	}
	return failed
}

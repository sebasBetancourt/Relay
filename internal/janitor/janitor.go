// Package janitor runs background cleanup jobs for the API process.
package janitor

import (
	"context"
	"log/slog"
	"time"
)

// Purger deletes expired rows in batches. *store.OAuthStates and
// *store.Idempotency implement it.
type Purger interface {
	DeleteExpired(ctx context.Context, limit int) (int64, error)
}

// Config for Run.
type Config struct {
	Log      *slog.Logger
	Name     string // for logs; default "oauth states"
	Purger   Purger
	Interval time.Duration // default 10m
	Batch    int           // default 1000
}

// Run purges once immediately, then every Interval, until ctx is cancelled.
// Errors are logged as Warn; the job never stops the process.
func Run(ctx context.Context, cfg Config) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	batch := cfg.Batch
	if batch <= 0 {
		batch = 1000
	}

	name := cfg.Name
	if name == "" {
		name = "oauth states"
	}

	purge(ctx, log, name, cfg.Purger, batch)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			purge(ctx, log, name, cfg.Purger, batch)
		}
	}
}

func purge(ctx context.Context, log *slog.Logger, name string, p Purger, batch int) {
	var total int64
	for {
		n, err := p.DeleteExpired(ctx, batch)
		if err != nil {
			log.Warn(name+" purge failed", "err", err)
			return
		}
		total += n
		if n < int64(batch) {
			break
		}
	}
	if total > 0 {
		log.Info(name+" purged", "deleted", total)
	}
}

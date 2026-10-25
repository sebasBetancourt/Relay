package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

const (
	defaultSyncInterval = 15 * time.Minute
	defaultSyncBatch    = 50
)

// StaleClaimer selects active accounts due for a periodic sync.
// *store.SyncStore implements it.
type StaleClaimer interface {
	ClaimStale(ctx context.Context, age time.Duration, limit int, fn func(ids []string) error) error
}

// ScheduleConfig for Run. The tick only enqueues. The NATS consumer syncs.
type ScheduleConfig struct {
	Log      *slog.Logger
	Claims   StaleClaimer
	Publish  Publisher
	Interval time.Duration // default 15m; also how old last_synced_at must be
	Batch    int           // default 50; one batch per tick
}

// Run enqueues accounts whose last sync is older than Interval. It runs
// one batch immediately and then on every tick until ctx is cancelled.
// A missed webhook is covered by the next tick. Publishing uses a msg id
// shared by replicas in the same bucket, so JetStream stores one job.
func Run(ctx context.Context, cfg ScheduleConfig) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultSyncInterval
	}
	batch := cfg.Batch
	if batch <= 0 {
		batch = defaultSyncBatch
	}
	n, err := enqueueStale(ctx, cfg.Claims, cfg.Publish, interval, batch)
	logSchedule(log, n, err)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := enqueueStale(ctx, cfg.Claims, cfg.Publish, interval, batch)
			logSchedule(log, n, err)
		}
	}
}

func enqueueStale(ctx context.Context, claims StaleClaimer, publish Publisher, interval time.Duration, batch int) (int, error) {
	if claims == nil || publish == nil {
		return 0, nil
	}
	var ids []string
	if err := claims.ClaimStale(ctx, interval, batch, func(got []string) error {
		ids = append([]string(nil), got...)
		return nil
	}); err != nil {
		return 0, err
	}
	bucket := time.Now().Unix() / int64(interval.Seconds())
	n := 0
	for _, id := range ids {
		body, err := json.Marshal(struct {
			AccountID string `json:"account_id"`
			Reason    string `json:"reason"`
		}{id, "periodic"})
		if err != nil {
			return n, err
		}
		msgID := fmt.Sprintf("periodic:%s:%d", id, bucket)
		if err := publish.Publish(ctx, subjectSync, msgID, body); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func logSchedule(log *slog.Logger, n int, err error) {
	if err != nil {
		log.Warn("calendar sync schedule failed", "err", err)
		return
	}
	if n > 0 {
		log.Info("calendar sync scheduled", "accounts", n)
	}
}

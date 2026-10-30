package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/notify"
	"github.com/sebasBetancourt/relay/internal/queue/natsq"
	"github.com/sebasBetancourt/relay/internal/testutil/fakegoogle"
	"github.com/sebasBetancourt/relay/internal/testutil/fakemicrosoft"
	"github.com/sebasBetancourt/relay/internal/testutil/natsjs"
)

func (f *flow) enableSync(t *testing.T) {
	t.Helper()
	c, err := natsq.Connect(f.ctx, natsjs.Start(t), natsq.WithMemory())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	f.nats = c
	f.syncOn = true
	f.router = f.buildRouter(f.pool)
	if f.syncSvc == nil {
		t.Fatal("sync service was not wired")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = c.Consume(ctx, notify.SubjectSync, f.syncSvc.HandleJob)
	}()
}

func TestDeltaSyncMatchesProvider(t *testing.T) {
	for _, provider := range providers {
		t.Run(provider, func(t *testing.T) {
			f := newFlow(t)
			f.enableSync(t)
			accountID := f.connectThrough(provider)
			f.waitEvents(accountID, map[string]bool{"ev-flow": false})

			start := time.Now().UTC().Add(3 * time.Hour).Truncate(time.Minute)
			switch provider {
			case "google":
				f.google.SetEvents(
					fakegoogle.Event{ID: "ev-flow", Summary: "Moved", Start: start.Format(time.RFC3339), End: start.Add(time.Hour).Format(time.RFC3339)},
					fakegoogle.Event{ID: "series_20261101T150000Z", Summary: "Instance", Start: start.Format(time.RFC3339), End: start.Add(time.Hour).Format(time.RFC3339)},
				)
			default:
				f.ms.SetEvents(
					fakemicrosoft.Event{ID: "ev-flow", Subject: "Moved", Start: start, End: start.Add(time.Hour)},
					fakemicrosoft.Event{ID: "series_20261101T150000Z", Subject: "Instance", Start: start, End: start.Add(time.Hour)},
				)
			}
			if err := f.syncSvc.Kick(f.ctx, accountID); err != nil {
				t.Fatal(err)
			}
			f.waitTitles(accountID, map[string]string{
				"ev-flow": "Moved", "series_20261101T150000Z": "Instance",
			})

			switch provider {
			case "google":
				f.google.SetEvents(fakegoogle.Event{
					ID: "series_20261101T150000Z", Summary: "Instance",
					Start: start.Format(time.RFC3339), End: start.Add(time.Hour).Format(time.RFC3339),
				})
			default:
				f.ms.SetEvents(fakemicrosoft.Event{
					ID: "series_20261101T150000Z", Subject: "Instance", Start: start, End: start.Add(time.Hour),
				})
			}
			if err := f.syncSvc.Kick(f.ctx, accountID); err != nil {
				t.Fatal(err)
			}
			f.waitEvents(accountID, map[string]bool{
				"ev-flow": true, "series_20261101T150000Z": false,
			})

			rec := f.do(http.MethodDelete, "/v1/accounts/"+accountID, f.authHdr())
			if rec.Code != http.StatusNoContent {
				t.Fatalf("disconnect %d %s", rec.Code, rec.Body.String())
			}
			var n int
			if err := f.pool.QueryRow(f.ctx, `
				SELECT
				  (SELECT count(*) FROM synced_events WHERE account_id = $1) +
				  (SELECT count(*) FROM account_sync_state WHERE account_id = $1)`, accountID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("calendar copy left behind: %d rows", n)
			}
		})
	}
}

func (f *flow) waitEvents(accountID string, deleted map[string]bool) {
	f.t.Helper()
	f.wait(accountID, func(got map[string]rowEvent) bool {
		if len(got) < len(deleted) {
			return false
		}
		for id, wantDel := range deleted {
			ev, ok := got[id]
			if !ok || ev.deleted != wantDel {
				return false
			}
		}
		return true
	})
}

func (f *flow) waitTitles(accountID string, titles map[string]string) {
	f.t.Helper()
	f.wait(accountID, func(got map[string]rowEvent) bool {
		for id, title := range titles {
			ev, ok := got[id]
			if !ok || ev.deleted || ev.title != title {
				return false
			}
		}
		return true
	})
}

type rowEvent struct {
	title   string
	deleted bool
}

func (f *flow) wait(accountID string, ok func(map[string]rowEvent) bool) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last map[string]rowEvent
	for time.Now().Before(deadline) {
		got, err := f.eventRows(accountID)
		if err != nil {
			f.t.Fatal(err)
		}
		last = got
		if ok(got) {
			var cursorLen int
			if err := f.pool.QueryRow(f.ctx, `SELECT char_length(cursor) FROM account_sync_state WHERE account_id = $1`, accountID).Scan(&cursorLen); err != nil {
				f.t.Fatal(err)
			}
			if cursorLen == 0 {
				f.t.Fatal("sync cursor is empty")
			}
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	raw, _ := json.Marshal(last)
	f.t.Fatalf("synced events = %s", raw)
}

func (f *flow) eventRows(accountID string) (map[string]rowEvent, error) {
	rows, err := f.pool.Query(f.ctx, `
		SELECT provider_event_id, deleted, data->>'title'
		FROM synced_events WHERE account_id = $1`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]rowEvent{}
	for rows.Next() {
		var id, title string
		var deleted bool
		if err := rows.Scan(&id, &deleted, &title); err != nil {
			return nil, err
		}
		out[id] = rowEvent{title: title, deleted: deleted}
	}
	return out, rows.Err()
}

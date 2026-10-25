package sync

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/notify"
	"github.com/sebasBetancourt/relay/internal/oauth"
	"github.com/sebasBetancourt/relay/internal/store"
	"github.com/sebasBetancourt/relay/internal/testutil/fakegoogle"
	"github.com/sebasBetancourt/relay/internal/testutil/fakemicrosoft"
)

func TestSubjectMatchesWebhook(t *testing.T) {
	if subjectSync != notify.SubjectSync {
		t.Fatalf("subject = %q, webhook publishes %q", subjectSync, notify.SubjectSync)
	}
}

func TestSyncAppliesDeltaAndResyncsOn410(t *testing.T) {
	g := fakegoogle.New(t, fakegoogle.Options{})
	g.IssueAccess("tok")
	start := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Minute)
	g.SetEvents(
		fakegoogle.Event{ID: "ev-1", Summary: "One", Start: start.Format(time.RFC3339), End: start.Add(time.Hour).Format(time.RFC3339)},
		fakegoogle.Event{ID: "series_20261101T150000Z", Summary: "Instance", Start: start.Format(time.RFC3339), End: start.Add(time.Hour).Format(time.RFC3339)},
	)
	mem := &memState{}
	src := NewGoogle(g.Client())
	src.BaseURL = g.APIBaseURL()
	svc := New(Deps{
		Log: discardLog(), Accounts: activeAccount("google"), Tokens: &memTokens{value: "tok"},
		Store: mem, Sources: map[string]DeltaSource{"google": src},
	})
	if err := svc.Sync(context.Background(), "acct-1"); err != nil {
		t.Fatal(err)
	}
	if mem.cursor == "" || len(mem.events) != 2 || mem.events["ev-1"].title != "One" {
		t.Fatalf("cursor empty=%v events=%v", mem.cursor == "", mem.events)
	}
	if mem.full != 1 {
		t.Fatalf("full syncs = %d", mem.full)
	}

	g.SetEvents(
		fakegoogle.Event{ID: "ev-1", Summary: "Moved", Start: start.Format(time.RFC3339), End: start.Add(time.Hour).Format(time.RFC3339)},
		fakegoogle.Event{ID: "series_20261101T150000Z", Summary: "Instance", Start: start.Format(time.RFC3339), End: start.Add(time.Hour).Format(time.RFC3339)},
	)
	if err := svc.Sync(context.Background(), "acct-1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(g.LastSyncQuery(), "syncToken=") {
		t.Fatalf("incremental query = %s", g.LastSyncQuery())
	}
	if mem.events["ev-1"].title != "Moved" || mem.events["series_20261101T150000Z"].deleted {
		t.Fatalf("after edit %+v", mem.events)
	}
	writes := mem.writes

	if err := svc.Sync(context.Background(), "acct-1"); err != nil {
		t.Fatal(err)
	}
	if mem.writes != writes {
		t.Fatalf("unchanged sync rewrote events: writes %d -> %d", writes, mem.writes)
	}

	g.ExpireSync()
	g.SetEvents(fakegoogle.Event{
		ID: "ev-2", Summary: "Fresh", Start: start.Format(time.RFC3339), End: start.Add(time.Hour).Format(time.RFC3339),
	})
	if err := svc.Sync(context.Background(), "acct-1"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(g.LastSyncQuery(), "syncToken=") {
		t.Fatalf("410 must full-sync, query = %s", g.LastSyncQuery())
	}
	if !mem.events["ev-1"].deleted || mem.events["ev-2"].title != "Fresh" {
		t.Fatalf("after 410 %+v", mem.events)
	}
}

func TestSyncWindowChangeDropsCursor(t *testing.T) {
	ms := fakemicrosoft.New(t, fakemicrosoft.Options{})
	ms.IssueAccess("tok")
	start := time.Now().UTC().Add(2 * time.Hour)
	ms.SetEvents(fakemicrosoft.Event{ID: "ev-1", Subject: "One", Start: start, End: start.Add(time.Hour)})
	mem := &memState{}
	src := NewMicrosoft(ms.Client())
	src.BaseURL = ms.GraphBaseURL()
	svc := New(Deps{
		Log: discardLog(), Accounts: activeAccount("microsoft"), Tokens: &memTokens{value: "tok"},
		Store: mem, Sources: map[string]DeltaSource{"microsoft": src},
	})
	if err := svc.Sync(context.Background(), "acct-1"); err != nil {
		t.Fatal(err)
	}
	svc.window = 7 * 24 * time.Hour
	ms.SetEvents(fakemicrosoft.Event{ID: "ev-1", Subject: "One", Start: start, End: start.Add(time.Hour)})
	if err := svc.Sync(context.Background(), "acct-1"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ms.LastDeltaURL(), "deltatoken") || !strings.Contains(ms.LastDeltaURL(), "startDateTime=") {
		t.Fatalf("window change query = %s", ms.LastDeltaURL())
	}
}

func TestSyncSkipsInactiveAndKeepsCursorOnFailure(t *testing.T) {
	g := fakegoogle.New(t, fakegoogle.Options{})
	g.IssueAccess("tok")
	mem := &memState{cursor: "keep"}
	src := NewGoogle(g.Client())
	src.BaseURL = g.APIBaseURL()
	acct := activeAccount("google")
	acct.status = "disconnected"
	svc := New(Deps{
		Log: discardLog(), Accounts: acct, Tokens: &memTokens{value: "tok"},
		Store: mem, Sources: map[string]DeltaSource{"google": src},
	})
	if err := svc.Sync(context.Background(), "acct-1"); err != nil {
		t.Fatal(err)
	}
	if g.Calls().Events != 0 || mem.cursor != "keep" {
		t.Fatalf("disconnected called the provider or moved the cursor")
	}

	acct.status = "active"
	g.FailNext("events", http.StatusInternalServerError)
	err := svc.Sync(context.Background(), "acct-1")
	if err == nil || mem.cursor != "keep" || mem.errMsg == "" {
		t.Fatalf("err %v cursor %q last %q", err, mem.cursor, mem.errMsg)
	}
}

func TestSyncRefreshesOnceOn401(t *testing.T) {
	g := fakegoogle.New(t, fakegoogle.Options{})
	start := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Minute)
	g.SetEvents(fakegoogle.Event{
		ID: "ev-1", Summary: "One", Start: start.Format(time.RFC3339), End: start.Add(time.Hour).Format(time.RFC3339),
	})
	tok := &memTokens{value: "stale", refreshed: "tok", onRefresh: func() { g.IssueAccess("tok") }}
	mem := &memState{}
	src := NewGoogle(g.Client())
	src.BaseURL = g.APIBaseURL()
	svc := New(Deps{
		Log: discardLog(), Accounts: activeAccount("google"), Tokens: tok,
		Store: mem, Sources: map[string]DeltaSource{"google": src},
	})
	if err := svc.Sync(context.Background(), "acct-1"); err != nil {
		t.Fatal(err)
	}
	if tok.refreshes != 1 || mem.events["ev-1"].title != "One" {
		t.Fatalf("refreshes %d events %+v", tok.refreshes, mem.events)
	}
}

type acctFixed struct {
	provider string
	status   string
}

func activeAccount(provider string) *acctFixed {
	return &acctFixed{provider: provider, status: "active"}
}

func (a *acctFixed) AccountByID(context.Context, string) (store.StoredAccount, error) {
	return store.StoredAccount{ID: "acct-1", TenantID: "tenant-1", Provider: a.provider, Status: a.status}, nil
}

type memTokens struct {
	value     string
	refreshed string
	refreshes int
	onRefresh func()
}

func (m *memTokens) AccessToken(context.Context, string, string) (oauth.AccessToken, error) {
	return oauth.AccessToken{Value: m.value, Provider: "google"}, nil
}

func (m *memTokens) RefreshAccessToken(context.Context, string, string) (oauth.AccessToken, error) {
	m.refreshes++
	if m.onRefresh != nil {
		m.onRefresh()
	}
	if m.refreshed != "" {
		m.value = m.refreshed
	}
	return oauth.AccessToken{Value: m.value}, nil
}

type memEvent struct {
	title   string
	version string
	deleted bool
}

type memState struct {
	cursor string
	window time.Duration
	events map[string]memEvent
	full   int
	writes int
	errMsg string
}

func (m *memState) State(context.Context, string) (store.SyncState, error) {
	end := time.Time{}
	if m.cursor != "" && m.window > 0 {
		end = time.Now().UTC().Add(m.window)
	}
	start := time.Time{}
	if !end.IsZero() {
		start = end.Add(-m.window)
	}
	return store.SyncState{Cursor: m.cursor, WindowStart: start, WindowEnd: end}, nil
}

func (m *memState) Apply(_ context.Context, req store.ApplyRequest) error {
	if req.Expect != m.cursor {
		return store.ErrSyncSuperseded
	}
	if m.events == nil {
		m.events = map[string]memEvent{}
	}
	seen := map[string]bool{}
	for _, ch := range req.Changes {
		var ev struct {
			Title string `json:"title"`
		}
		_ = json.Unmarshal(ch.Data, &ev)
		prev, ok := m.events[ch.ProviderEventID]
		if !ok || prev.version != ch.Version || prev.deleted != ch.Deleted {
			m.writes++
			m.events[ch.ProviderEventID] = memEvent{title: ev.Title, version: ch.Version, deleted: ch.Deleted}
		}
		if !ch.Deleted {
			seen[ch.ProviderEventID] = true
		}
	}
	if req.Full {
		m.full++
		for id, ev := range m.events {
			if !seen[id] && !ev.deleted {
				ev.deleted = true
				m.events[id] = ev
			}
		}
	}
	m.cursor = req.Next
	m.window = req.WindowEnd.Sub(req.WindowStart)
	return nil
}

func (m *memState) MarkError(context.Context, string, string) error {
	m.errMsg = "failed"
	return nil
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

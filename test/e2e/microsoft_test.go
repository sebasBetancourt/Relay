package e2e

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/testutil/fakemicrosoft"
)

// Microsoft rotates the refresh token on every refresh, and the fake kills
// the old one at once: the token Relay stores must always be the newest.
func TestMicrosoftRefreshRotation(t *testing.T) {
	f := newFlow(t)
	accountID := f.connectThrough("microsoft")

	for i := 1; i <= 3; i++ {
		before := f.storedRefresh("microsoft", accountID)
		f.ms.ExpireAccessTokens()
		f.expireInDB(accountID)

		if rec := f.events(accountID, ""); rec.Code != http.StatusOK {
			t.Fatalf("refresh %d: events %d %s", i, rec.Code, rec.Body.String())
		}
		after := f.storedRefresh("microsoft", accountID)
		if after == before {
			t.Fatalf("refresh %d: the stored refresh token did not change", i)
		}
		if f.ms.RefreshTokenValid(before) || !f.ms.RefreshTokenValid(after) {
			t.Fatalf("refresh %d: Relay must hold the only valid refresh token", i)
		}
	}
	if n := f.ms.Calls().Refresh; n != 3 {
		t.Fatalf("refreshes = %d, want 3", n)
	}
}

// Two API replicas (own pools, own singleflight) see the same expired token
// at once. Without the row lock both would redeem the refresh token and the
// second would get invalid_grant from the strict fake.
func TestMicrosoftConcurrentReplicas(t *testing.T) {
	f := newFlow(t)
	accountID := f.connectThrough("microsoft")
	routers := []http.Handler{f.router, f.replica()}
	// Real Entra ID takes hundreds of ms: keep both replicas inside the
	// refresh at the same time, or there is no race to prevent.
	f.ms.SetRefreshLatency(200 * time.Millisecond)
	f.ms.ExpireAccessTokens()
	f.expireInDB(accountID)
	before := f.ms.Calls().Refresh

	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for i := range 8 {
		wg.Add(1)
		go func(r http.Handler) {
			defer wg.Done()
			codes <- doOn(r, http.MethodGet, "/v1/calendars/"+accountID+"/events", f.authHdr()).Code
		}(routers[i%2])
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusOK {
			t.Fatalf("a request got %d, want every one 200", c)
		}
	}
	if d := f.ms.Calls().Refresh - before; d != 1 {
		t.Fatalf("refreshes = %d, want exactly 1 across both replicas", d)
	}
	if f.row(accountID).Status != "active" {
		t.Fatal("the account must stay active")
	}
}

// Paging through Graph: cancelled occurrences are dropped, no page is
// empty, nothing is lost or repeated, and only $skiptoken is reused (the
// fake's nextLink points at the real Graph host).
func TestMicrosoftPaging(t *testing.T) {
	f := newFlow(t)
	accountID := f.connectThrough("microsoft")

	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	evs := []fakemicrosoft.Event{
		{ID: "cancel-1", Subject: "Cancelled", Start: day.Add(8 * time.Hour), End: day.Add(9 * time.Hour), Cancelled: true},
		{ID: "cancel-2", Subject: "Cancelled", Start: day.Add(8 * time.Hour), End: day.Add(9 * time.Hour), Cancelled: true},
		{ID: "holiday", Subject: "Holiday", Start: day, End: day.AddDate(0, 0, 1), AllDay: true, TimeZone: "Romance Standard Time"},
	}
	for i := range 5 {
		start := day.Add(time.Duration(10+i) * time.Hour)
		evs = append(evs, fakemicrosoft.Event{
			ID: fmt.Sprintf("m%d", i), Subject: fmt.Sprintf("Meeting %d", i),
			Start: start, End: start.Add(30 * time.Minute), TimeZone: "SA Pacific Standard Time",
		})
	}
	f.ms.SetEvents(evs...)

	seen := map[string]bool{}
	query := "time_min=2026-11-01T00:00:00Z&time_max=2026-11-10T00:00:00Z&max_results=2"
	for pages := 1; ; pages++ {
		if pages > 10 {
			t.Fatal("paging did not end")
		}
		rec := f.events(accountID, query)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: %d %s", pages, rec.Code, rec.Body.String())
		}
		var page eventsPage
		_ = json.Unmarshal(rec.Body.Bytes(), &page)
		if len(page.Events) == 0 && page.NextPageToken != "" {
			t.Fatalf("page %d is empty but not the last one", pages)
		}
		for _, e := range page.Events {
			if seen[e.ID] {
				t.Fatalf("event %s repeated", e.ID)
			}
			seen[e.ID] = true
			switch {
			case e.ID == "holiday" && (!e.AllDay || e.Start != "2026-11-02" || e.End != "2026-11-03"):
				t.Fatalf("all-day event = %+v", e)
			case e.ID == "m0" && (e.Start != "2026-11-02T10:00:00Z" || e.Timezone != "America/Bogota"):
				t.Fatalf("timed event = %+v", e)
			}
		}
		if page.NextPageToken == "" {
			break
		}
		query = "page_token=" + url.QueryEscape(page.NextPageToken)
	}
	if len(seen) != 6 || seen["cancel-1"] || seen["cancel-2"] {
		t.Fatalf("events seen = %v, want the 6 not cancelled", seen)
	}

	// A page_token minted for another provider is rejected before any call.
	googleToken := base64.RawURLEncoding.EncodeToString([]byte(
		`{"v":1,"p":"google","t":"g-page","min":"2026-11-01T00:00:00Z","max":"2026-11-10T00:00:00Z","n":2}`))
	before := f.ms.Calls().Events
	if rec := f.events(accountID, "page_token="+googleToken); !hasError(rec, http.StatusBadRequest, "invalid_query") {
		t.Fatalf("foreign page_token: %d %s", rec.Code, rec.Body.String())
	}
	if f.ms.Calls().Events != before {
		t.Fatal("a rejected page_token must not reach Graph")
	}
}

// interaction_required (MFA, Conditional Access): only a new connect helps.
func TestMicrosoftInteractionRequired(t *testing.T) {
	f := newFlow(t)
	accountID := f.connectThrough("microsoft")
	f.ms.FailNextRefresh("interaction_required")
	f.expireInDB(accountID)

	if rec := f.events(accountID, ""); !hasError(rec, http.StatusConflict, "account_revoked") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if f.row(accountID).Status != "revoked" {
		t.Fatal("the account must be revoked")
	}
	if id := f.connectThrough("microsoft"); id != accountID {
		t.Fatalf("reconnect account_id = %s, want %s", id, accountID)
	}
	if rec := f.events(accountID, ""); rec.Code != http.StatusOK {
		t.Fatalf("after reconnect %d %s", rec.Code, rec.Body.String())
	}
}

// A Microsoft account without an Outlook mailbox is not a transient error.
func TestMicrosoftMailboxNotEnabled(t *testing.T) {
	f := newFlow(t)
	accountID := f.connectThrough("microsoft")
	f.ms.MailboxNotEnabled(true)

	if rec := f.events(accountID, ""); !hasError(rec, http.StatusConflict, "calendar_unavailable") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if f.row(accountID).Status != "active" {
		t.Fatal("no mailbox is not a revocation")
	}
}

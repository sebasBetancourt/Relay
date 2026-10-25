package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/store"
	"github.com/sebasBetancourt/relay/internal/testutil/fakegoogle"
	"github.com/sebasBetancourt/relay/internal/testutil/fakemicrosoft"
)

func TestSubscribeRenewAndStopGoogle(t *testing.T) {
	g := fakegoogle.New(t, fakegoogle.Options{})
	g.IssueAccess("test-access")
	subs := newMemSubs()
	acct := store.StoredAccount{ID: "acct-1", TenantID: "tenant-1", Provider: "google", Status: "active", HasTokens: true}
	svc := New(Deps{
		Accounts: memAcct{a: acct}, Tokens: memTok{value: "test-access"}, Subs: subs,
		BaseURL: "https://relay.example",
		Providers: map[string]Provider{
			"google": &Google{BaseURL: g.APIBaseURL(), HTTP: g.Client()},
		},
	})

	if err := svc.Subscribe(context.Background(), acct.ID); err != nil {
		t.Fatal(err)
	}
	if g.Watches() != 1 {
		t.Fatalf("watches %d", g.Watches())
	}
	got, err := subs.ByAccount(context.Background(), acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	watch := g.LastWatch()
	if got.SecretHash == watch.Token || got.SecretHash != HashSecret(watch.Token) {
		t.Fatal("secret must be stored as a hash")
	}
	if !got.ExpiresAt.After(time.Now()) || got.ResourceID == "" {
		t.Fatalf("row %+v", got)
	}
	before := subs.upserts

	if err := svc.RenewAccount(context.Background(), acct.ID); err != nil {
		t.Fatal(err)
	}
	if g.Watches() != 2 || g.Stops() != 1 {
		t.Fatalf("watches %d stops %d", g.Watches(), g.Stops())
	}
	again, _ := subs.ByAccount(context.Background(), acct.ID)
	if again.ExternalID == watch.ID || again.SecretHash != HashSecret(g.LastWatch().Token) {
		t.Fatal("renew must replace the channel")
	}

	g.FailNext("watch", http.StatusInternalServerError)
	if err := svc.RenewAccount(context.Background(), acct.ID); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if subs.upserts != before+1 {
		t.Fatalf("upserts %d, a 5xx must leave the row", subs.upserts)
	}
	kept, _ := subs.ByAccount(context.Background(), acct.ID)
	if kept.ExternalID != again.ExternalID {
		t.Fatal("5xx rewrote the subscription")
	}

	g.FailNext("stop", http.StatusInternalServerError)
	if err := svc.Stop(context.Background(), acct.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := subs.ByAccount(context.Background(), acct.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("stop must delete the row even when the provider errors")
	}
}

func TestSubscribeMicrosoftHandshake(t *testing.T) {
	ms := fakemicrosoft.New(t, fakemicrosoft.Options{})
	ms.IssueAccess("test-access")
	var svc atomic.Pointer[Service]
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		svc.Load().Microsoft(w, r)
	}))
	t.Cleanup(relay.Close)

	subs := newMemSubs()
	acct := store.StoredAccount{ID: "acct-ms", TenantID: "tenant-1", Provider: "microsoft", Status: "active", HasTokens: true}
	s := New(Deps{
		Accounts: memAcct{a: acct}, Tokens: memTok{value: "test-access"}, Subs: subs,
		Publish: &memPub{}, BaseURL: relay.URL,
		Providers: map[string]Provider{
			"microsoft": &Microsoft{BaseURL: ms.GraphBaseURL(), HTTP: ms.Client()},
		},
	})
	svc.Store(s)
	if err := s.Subscribe(context.Background(), acct.ID); err != nil {
		t.Fatal(err)
	}
	if ms.Subscriptions() != 1 {
		t.Fatalf("subscriptions %d", ms.Subscriptions())
	}
	row, err := subs.ByAccount(context.Background(), acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := ms.LastSubscription()
	if row.SecretHash == last.ClientState || !row.ExpiresAt.After(time.Now()) {
		t.Fatalf("row hash stored plaintext or expiry in the past: %+v", row)
	}
	if last.Expiration.Before(time.Now().Add(6 * 24 * time.Hour)) {
		t.Fatalf("expiration %s is far below the 7 day cap", last.Expiration)
	}
}

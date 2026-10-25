package e2e

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/refresher"
)

// The background worker and a lazy request hit the same expired Microsoft
// account together. The row lock allows one refresh; the account stays active.
func TestRefresherRacesLazyRequestOnce(t *testing.T) {
	f := newFlow(t)
	accountID := f.connectThrough("microsoft")
	f.ms.SetRefreshLatency(200 * time.Millisecond)
	f.ms.ExpireAccessTokens()
	f.expireInDB(accountID)
	before := f.ms.Calls().Refresh

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		refresher.Run(ctx, refresher.Config{
			Log: discardLogger(), Accounts: f.accounts, Refresh: f.svc,
			Interval: time.Hour, Ahead: 10 * time.Minute, Concurrency: 2, Batch: 10,
		})
	}()
	if rec := f.events(accountID, ""); rec.Code != http.StatusOK {
		t.Fatalf("events %d %s", rec.Code, rec.Body.String())
	}
	cancel()
	wg.Wait()

	if d := f.ms.Calls().Refresh - before; d != 1 {
		t.Fatalf("refreshes = %d, want exactly 1", d)
	}
	if f.row(accountID).Status != "active" {
		t.Fatal("the account must stay active")
	}
}

// Two RefreshAccount calls on the same account: the first rotates, the
// second sees an expiry past ahead under the row lock and does not.
func TestRefreshAccountTwiceRefreshesOnce(t *testing.T) {
	f := newFlow(t)
	accountID := f.connectThrough("microsoft")
	f.ms.ExpireAccessTokens()
	f.expireInDB(accountID)
	before := f.ms.Calls().Refresh

	ahead := 10 * time.Minute
	if err := f.svc.RefreshAccount(context.Background(), accountID, ahead); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RefreshAccount(context.Background(), accountID, ahead); err != nil {
		t.Fatal(err)
	}
	if d := f.ms.Calls().Refresh - before; d != 1 {
		t.Fatalf("refreshes = %d, want exactly 1", d)
	}
	if f.row(accountID).Status != "active" {
		t.Fatal("the account must stay active")
	}
}

func TestRefresherRevokesInvalidGrant(t *testing.T) {
	f := newFlow(t)
	accountID := f.connectThrough("microsoft")
	f.ms.FailNextRefresh("invalid_grant")
	f.expireInDB(accountID)

	err := f.svc.RefreshAccount(context.Background(), accountID, 10*time.Minute)
	if err == nil {
		t.Fatal("expected revocation")
	}
	if f.row(accountID).Status != "revoked" || f.row(accountID).HasTokens {
		t.Fatal("invalid_grant must revoke and drop tokens")
	}
}

func TestRefresherLeavesAccountOnProvider5xx(t *testing.T) {
	f := newFlow(t)
	accountID := f.connectThrough("microsoft")
	f.ms.FailNext("token", http.StatusInternalServerError)
	f.expireInDB(accountID)

	err := f.svc.RefreshAccount(context.Background(), accountID, 10*time.Minute)
	if err == nil {
		t.Fatal("expected provider error")
	}
	if f.row(accountID).Status != "active" || !f.row(accountID).HasTokens {
		t.Fatal("a 5xx must not revoke")
	}
}

func TestRefresherSkipsDisconnected(t *testing.T) {
	f := newFlow(t)
	accountID := f.connectThrough("microsoft")
	if _, err := f.accounts.Disconnect(context.Background(), accountID, f.tenant.ID); err != nil {
		t.Fatal(err)
	}
	before := f.ms.Calls().Refresh
	err := f.svc.RefreshAccount(context.Background(), accountID, 10*time.Minute)
	if err == nil {
		t.Fatal("expected disconnected")
	}
	if f.row(accountID).Status != "disconnected" {
		t.Fatal("a disconnect mid-cycle must not become revoked")
	}
	if f.ms.Calls().Refresh != before {
		t.Fatal("must not call the provider")
	}
}

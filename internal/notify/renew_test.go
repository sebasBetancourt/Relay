package notify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type fakeClaims struct {
	expiring []string
	missing  []string
	calls    int
}

func (f *fakeClaims) ClaimExpiring(_ context.Context, _ time.Duration, limit int, fn func([]string) error) error {
	f.calls++
	ids := append([]string(nil), f.expiring...)
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	f.expiring = nil
	return fn(ids)
}

func (f *fakeClaims) ClaimMissing(_ context.Context, limit int, fn func([]string) error) error {
	ids := append([]string(nil), f.missing...)
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	f.missing = nil
	return fn(ids)
}

type fakeMaintain struct {
	mu       sync.Mutex
	renewErr map[string]error
	renewed  []string
	subbed   []string
}

func (f *fakeMaintain) RenewAccount(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewed = append(f.renewed, id)
	return f.renewErr[id]
}

func (f *fakeMaintain) Subscribe(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subbed = append(f.subbed, id)
	return nil
}

func TestCycleContinuesAfterRenewError(t *testing.T) {
	claims := &fakeClaims{expiring: []string{"a", "b"}, missing: []string{"c"}}
	maint := &fakeMaintain{renewErr: map[string]error{"a": errors.New("provider 503")}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := cycle(context.Background(), log, claims, maint, 24*time.Hour, 10, 2)
	if st.renewed != 1 || st.failed != 1 || st.subscribed != 1 {
		t.Fatalf("stats %+v renewed %v subbed %v", st, maint.renewed, maint.subbed)
	}
	if len(maint.renewed) != 2 {
		t.Fatalf("renewed %v, both accounts must be attempted", maint.renewed)
	}
}

func TestCycleCancelled(t *testing.T) {
	claims := &fakeClaims{expiring: []string{"a"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := cycle(ctx, log, claims, &fakeMaintain{}, time.Hour, 10, 1)
	if st.renewed != 0 || claims.calls != 0 {
		t.Fatalf("stats %+v calls %d", st, claims.calls)
	}
}

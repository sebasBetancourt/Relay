package refresher

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/oauth"
	"github.com/sebasBetancourt/relay/internal/store"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeClaim struct {
	ids []string
	err error
	mu  sync.Mutex
	n   atomic.Int32
}

func (f *fakeClaim) ClaimExpiring(_ context.Context, _ time.Duration, limit int, fn func([]string) error) error {
	f.n.Add(1)
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	ids := append([]string(nil), f.ids...)
	f.ids = nil // one batch, then empty
	f.mu.Unlock()
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return fn(ids)
}

type fakeRefresh struct {
	mu       sync.Mutex
	errFor   map[string]error
	called   []string
	inFlight atomic.Int32
	max      atomic.Int32
	release  chan struct{}
	started  atomic.Int32
}

func (f *fakeRefresh) RefreshAccount(ctx context.Context, id string, _ time.Duration) error {
	n := f.inFlight.Add(1)
	for {
		old := f.max.Load()
		if n <= old || f.max.CompareAndSwap(old, n) {
			break
		}
	}
	f.started.Add(1)
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			f.inFlight.Add(-1)
			return ctx.Err()
		}
	}
	f.inFlight.Add(-1)

	f.mu.Lock()
	f.called = append(f.called, id)
	err := f.errFor[id]
	f.mu.Unlock()
	return err
}

func TestCycleContinuesAfterOneError(t *testing.T) {
	claim := &fakeClaim{ids: []string{"a", "b", "c", "d"}}
	refresh := &fakeRefresh{errFor: map[string]error{
		"b": oauth.ErrAccountRevoked,
		"c": errors.New("provider 500"),
		"d": store.ErrNotFound,
	}}
	st := cycle(context.Background(), discardLog(), claim, refresh, time.Minute, 10, 4)
	if st.refreshed != 1 || st.revoked != 1 || st.failed != 1 {
		t.Fatalf("stats = %+v, want refreshed 1 revoked 1 failed 1", st)
	}
	if len(refresh.called) != 4 {
		t.Fatalf("called %v, one error must not skip the rest", refresh.called)
	}
}

func TestCycleLimitsConcurrency(t *testing.T) {
	claim := &fakeClaim{ids: []string{"a", "b", "c", "d"}}
	release := make(chan struct{})
	refresh := &fakeRefresh{release: release}
	done := make(chan stats, 1)
	go func() {
		done <- cycle(context.Background(), discardLog(), claim, refresh, time.Minute, 10, 2)
	}()

	deadline := time.After(2 * time.Second)
	for refresh.started.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("started = %d", refresh.started.Load())
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	time.Sleep(30 * time.Millisecond)
	if got := refresh.started.Load(); got != 2 {
		t.Fatalf("started = %d, want the limit of 2 held until release", got)
	}
	if refresh.max.Load() != 2 {
		t.Fatalf("max in flight = %d, want 2", refresh.max.Load())
	}
	close(release)
	select {
	case st := <-done:
		if st.refreshed != 4 {
			t.Fatalf("stats = %+v", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cycle did not finish")
	}
	if refresh.max.Load() != 2 {
		t.Fatalf("max in flight = %d, want 2", refresh.max.Load())
	}
}

func TestCycleClaimErrorSkipsRefresh(t *testing.T) {
	claim := &fakeClaim{err: errors.New("db down")}
	refresh := &fakeRefresh{}
	st := cycle(context.Background(), discardLog(), claim, refresh, time.Minute, 10, 2)
	if st != (stats{}) || len(refresh.called) != 0 {
		t.Fatalf("stats=%+v called=%v", st, refresh.called)
	}
}

func TestCycleCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	claim := &fakeClaim{ids: []string{"a"}}
	refresh := &fakeRefresh{}
	st := cycle(ctx, discardLog(), claim, refresh, time.Minute, 10, 2)
	if st != (stats{}) || claim.n.Load() != 0 {
		t.Fatalf("cancelled cycle must not claim, stats=%+v claims=%d", st, claim.n.Load())
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	claim := &fakeClaim{ids: []string{"a"}}
	refresh := &fakeRefresh{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Config{
			Log: discardLog(), Accounts: claim, Refresh: refresh,
			Interval: time.Hour, Ahead: time.Minute, Concurrency: 2, Batch: 10,
		})
		close(done)
	}()
	deadline := time.After(2 * time.Second)
	for claim.n.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("Run did not claim on start")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
}

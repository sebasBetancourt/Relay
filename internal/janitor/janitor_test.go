package janitor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakePurger struct {
	mu      sync.Mutex
	batches []int64
	errAt   int // 1-based call index that fails; 0 = never
	calls   atomic.Int32
}

func (f *fakePurger) DeleteExpired(context.Context, int) (int64, error) {
	n := int(f.calls.Add(1))
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errAt == n {
		return 0, errors.New("boom")
	}
	if len(f.batches) == 0 {
		return 0, nil
	}
	v := f.batches[0]
	f.batches = f.batches[1:]
	return v, nil
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func waitCalls(t *testing.T, p *fakePurger, want int32) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for p.calls.Load() < want {
		select {
		case <-deadline:
			t.Fatalf("calls = %d, want >= %d", p.calls.Load(), want)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestRunPurgesOnStartAndChainsBatches(t *testing.T) {
	p := &fakePurger{batches: []int64{1000, 1000, 7}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Config{Log: discardLog(), Purger: p, Interval: time.Hour, Batch: 1000})
		close(done)
	}()
	waitCalls(t, p, 3)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
	if p.calls.Load() != 3 {
		t.Fatalf("calls = %d", p.calls.Load())
	}
}

func TestRunSurvivesErrorAndStopsOnCancel(t *testing.T) {
	p := &fakePurger{errAt: 1, batches: []int64{3}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Config{Log: discardLog(), Purger: p, Interval: 30 * time.Millisecond, Batch: 1000})
		close(done)
	}()
	// First call errors on start; after Interval the next tick succeeds with batch 3 then 0.
	waitCalls(t, p, 2)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

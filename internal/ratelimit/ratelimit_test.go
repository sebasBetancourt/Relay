package ratelimit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

var ctx = context.Background()

func newMiniredis(t *testing.T) (*miniredis.Miniredis, *Redis) {
	t.Helper()
	srv := miniredis.RunT(t)
	srv.SetTime(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC))
	r, err := OpenRedis("redis://" + srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return srv, r
}

func allowed(t *testing.T, l Limiter, key string, limit Limit) Result {
	t.Helper()
	res, err := l.Allow(ctx, key, limit)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// Both limiters must make the same decisions; only the storage differs.
func TestLimitersBurstThenRefuse(t *testing.T) {
	_, r := newMiniredis(t)
	frozen := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	local := NewLocal()
	local.now = func() time.Time { return frozen }

	for name, l := range map[string]Limiter{"redis": r, "local": local} {
		t.Run(name, func(t *testing.T) {
			limit := Limit{PerMinute: 60, Burst: 3}
			for want := 2; want >= 0; want-- {
				res := allowed(t, l, "k", limit)
				if !res.Allowed || res.Remaining != want {
					t.Fatalf("res = %+v, want allowed with %d remaining", res, want)
				}
			}
			res := allowed(t, l, "k", limit)
			if res.Allowed {
				t.Fatal("request past the burst was allowed")
			}
			// 60 per minute is one per second.
			if res.RetryAfter <= 0 || res.RetryAfter > time.Second {
				t.Fatalf("RetryAfter = %v, want (0, 1s]", res.RetryAfter)
			}
			if other := allowed(t, l, "other", limit); !other.Allowed {
				t.Fatal("one key must not spend another key's budget")
			}
		})
	}
}

func TestRedisRecoversWithItsOwnClock(t *testing.T) {
	srv, r := newMiniredis(t)
	limit := Limit{PerMinute: 60, Burst: 1}
	start := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	allowed(t, r, "k", limit)
	res := allowed(t, r, "k", limit)
	if res.Allowed {
		t.Fatal("second request must wait")
	}
	srv.SetTime(start.Add(res.RetryAfter))
	if res := allowed(t, r, "k", limit); !res.Allowed {
		t.Fatalf("after RetryAfter the request must pass: %+v", res)
	}
}

func TestRedisKeysExpire(t *testing.T) {
	srv, r := newMiniredis(t)
	allowed(t, r, "k", Limit{PerMinute: 60, Burst: 5})
	keys := srv.Keys()
	if len(keys) != 1 || srv.TTL(keys[0]) <= 0 {
		t.Fatalf("keys %v must carry a TTL so idle tenants free memory", keys)
	}
}

// A failed Redis costs the one request that finds out a few milliseconds, not
// the client's timeout. Nothing listens on this port.
func TestRedisDownFailsFast(t *testing.T) {
	r, err := OpenRedis("redis://127.0.0.1:1/0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	start := time.Now()
	if _, err := r.Allow(ctx, "k", Limit{PerMinute: 60, Burst: 5}); err == nil {
		t.Fatal("expected an error")
	}
	if took := time.Since(start); took > 200*time.Millisecond {
		t.Fatalf("Allow took %v with Redis down", took)
	}
}

func TestOpenRedisErrorHidesURL(t *testing.T) {
	_, err := OpenRedis("redis://user:s3cret@host:notaport")
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("err = %v", err)
	}
}

// A new limit changes the refill rate; it does not hand out free tokens.
func TestLocalAppliesChangedLimit(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	local := NewLocal()
	local.now = func() time.Time { return now }

	allowed(t, local, "k", Limit{PerMinute: 60, Burst: 1})
	if allowed(t, local, "k", Limit{PerMinute: 60, Burst: 1}).Allowed {
		t.Fatal("burst of 1 is spent")
	}
	raised := Limit{PerMinute: 600, Burst: 1}
	if allowed(t, local, "k", raised).Allowed {
		t.Fatal("raising the limit must not refill the budget")
	}
	now = now.Add(100 * time.Millisecond) // one token at 600/min, a tenth at 60/min
	if !allowed(t, local, "k", raised).Allowed {
		t.Fatal("a raised override must apply to a live entry")
	}
}

func TestLocalKeepsMemoryBounded(t *testing.T) {
	local := NewLocal()
	limit := Limit{PerMinute: 60, Burst: 1}
	for i := range maxLocalEntries + 10 {
		allowed(t, local, fmt.Sprintf("k%d", i), limit)
	}
	if n := len(local.entries); n > maxLocalEntries {
		t.Fatalf("entries = %d, want at most %d", n, maxLocalEntries)
	}
}

type fakePrimary struct {
	err   error
	calls atomic.Int32
}

func (f *fakePrimary) Allow(context.Context, string, Limit) (Result, error) {
	f.calls.Add(1)
	return Result{Allowed: true, Remaining: 42}, f.err
}

func TestFailoverUsesPrimaryWhileHealthy(t *testing.T) {
	var logs bytes.Buffer
	f := NewFailover(slog.New(slog.NewTextHandler(&logs, nil)), &fakePrimary{}, NewLocal())
	if res := allowed(t, f, "k", Limit{PerMinute: 60, Burst: 5}); res.Remaining != 42 {
		t.Fatalf("res = %+v, want the primary's answer", res)
	}
	if logs.Len() != 0 {
		t.Fatalf("unexpected log: %s", logs.String())
	}
}

func TestFailoverFailsOpenThenRetriesPrimary(t *testing.T) {
	var logs bytes.Buffer
	primary := &fakePrimary{err: errors.New("redis down")}
	f := NewFailover(slog.New(slog.NewTextHandler(&logs, nil)), primary, NewLocal())
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	f.now = func() time.Time { return now }
	limit := Limit{PerMinute: 60, Burst: 5}

	for range 3 {
		if res := allowed(t, f, "k", limit); !res.Allowed {
			t.Fatal("must fail open")
		}
	}
	if got := primary.calls.Load(); got != 1 {
		t.Fatalf("primary calls = %d, want 1: the cooldown must spare a failing Redis", got)
	}
	if got := strings.Count(logs.String(), "local fallback"); got != 1 {
		t.Fatalf("warnings = %d, want 1 per cooldown:\n%s", got, logs.String())
	}

	primary.err = nil
	now = now.Add(failoverCooldown)
	if res := allowed(t, f, "k", limit); res.Remaining != 42 {
		t.Fatalf("res = %+v, want the primary back after the cooldown", res)
	}
}

func TestFailoverLocalStillLimits(t *testing.T) {
	f := NewFailover(slog.New(slog.DiscardHandler), &fakePrimary{err: errors.New("down")}, NewLocal())
	limit := Limit{PerMinute: 1, Burst: 2}
	allowed(t, f, "k", limit)
	allowed(t, f, "k", limit)
	if allowed(t, f, "k", limit).Allowed {
		t.Fatal("the fallback is a limit, not an open door")
	}
}

func TestFailoverIgnoresClientCancel(t *testing.T) {
	var logs bytes.Buffer
	primary := &fakePrimary{err: context.Canceled}
	f := NewFailover(slog.New(slog.NewTextHandler(&logs, nil)), primary, NewLocal())
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.Allow(cancelled, "k", Limit{PerMinute: 60, Burst: 5}); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 || f.retryAt.Load() != 0 {
		t.Fatalf("a client hanging up must not trip the failover: %s", logs.String())
	}
}

package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"
)

// redisTimeout bounds every Redis step. The limiter sits on the hot path of
// every request: a slow Redis must cost milliseconds, not the client's timeout.
const redisTimeout = 100 * time.Millisecond

// Redis is a Limiter shared by all replicas. redis_rate runs GCRA in one Lua
// script: the check and the spend are atomic, and it reads Redis's clock, so
// skewed replica clocks do not matter. Its keys carry a TTL, so idle tenants
// do not hold memory.
type Redis struct {
	client  *redis.Client
	limiter *redis_rate.Limiter
}

// OpenRedis parses url without dialing: a Redis that is down at startup must
// not stop the API. The error never includes url, which may hold a password.
func OpenRedis(url string) (*Redis, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: invalid format")
	}
	opts.DialTimeout = redisTimeout
	opts.ReadTimeout = redisTimeout
	opts.WriteTimeout = redisTimeout
	// Failover decides what a failure means, so neither layer retries here.
	// The dial default alone (5 tries, 100ms apart) would stall a request 500ms.
	opts.MaxRetries = -1
	opts.DialerRetries = 1
	client := redis.NewClient(opts)
	return &Redis{client: client, limiter: redis_rate.NewLimiter(client)}, nil
}

// Close releases the connection pool.
func (r *Redis) Close() error { return r.client.Close() }

func (r *Redis) Allow(ctx context.Context, key string, limit Limit) (Result, error) {
	res, err := r.limiter.Allow(ctx, key, redis_rate.Limit{
		Rate: limit.PerMinute, Burst: limit.Burst, Period: time.Minute,
	})
	if err != nil {
		return Result{}, fmt.Errorf("redis rate limit: %w", err)
	}
	out := Result{Allowed: res.Allowed > 0, Remaining: res.Remaining, ResetAfter: res.ResetAfter}
	if !out.Allowed {
		out.RetryAfter = res.RetryAfter
	}
	return out, nil
}

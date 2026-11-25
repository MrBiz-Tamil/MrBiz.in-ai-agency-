// Package ratelimit bounds how fast one tenant's agents can call tools. Counters are keyed per tenant, so one
// business's runaway agent cannot starve another.
package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Limiter decides whether a tenant may make another call right now.
type Limiter interface {
	// Allow records one call. When it returns ok=false, retryAfter says how long until the window resets.
	Allow(ctx context.Context, tenant uuid.UUID) (ok bool, retryAfter time.Duration, err error)
}

// Redis is a fixed-window limiter shared by every bus replica. Keys follow the platform convention
// t:{tenant_id}:... and expire on their own.
type Redis struct {
	Client *redis.Client
	Limit  int64
	Window time.Duration
	Now    func() time.Time
}

func (r *Redis) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Redis) Allow(ctx context.Context, tenant uuid.UUID) (bool, time.Duration, error) {
	if r.Limit <= 0 || r.Window <= 0 {
		return false, 0, fmt.Errorf("ratelimit: limit and window must be positive")
	}
	now := r.now()
	bucket := now.UnixNano() / int64(r.Window)
	key := fmt.Sprintf("t:%s:mcp:rl:%d", tenant, bucket)
	pipe := r.Client.TxPipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, 2*r.Window)
	if _, err := pipe.Exec(ctx); err != nil {
		return false, 0, fmt.Errorf("ratelimit: %w", err)
	}
	if incr.Val() > r.Limit {
		reset := time.Unix(0, (bucket+1)*int64(r.Window))
		return false, reset.Sub(now), nil
	}
	return true, 0, nil
}

// Memory is a single-process fixed-window limiter for tests and local development.
type Memory struct {
	Limit  int64
	Window time.Duration
	Now    func() time.Time

	mu      sync.Mutex
	buckets map[string]int64
}

func (m *Memory) Allow(_ context.Context, tenant uuid.UUID) (bool, time.Duration, error) {
	now := time.Now()
	if m.Now != nil {
		now = m.Now()
	}
	bucket := now.UnixNano() / int64(m.Window)
	key := fmt.Sprintf("%s:%d", tenant, bucket)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.buckets == nil {
		m.buckets = map[string]int64{}
	}
	m.buckets[key]++
	if m.buckets[key] > m.Limit {
		return false, time.Unix(0, (bucket+1)*int64(m.Window)).Sub(now), nil
	}
	return true, 0, nil
}

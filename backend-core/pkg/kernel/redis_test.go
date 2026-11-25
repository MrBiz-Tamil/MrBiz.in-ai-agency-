package kernel

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
)

// These tests exercise the Redis code paths (including the Lua budget script) against an
// in-process Redis server, so they run everywhere without external services.

func newRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb, mr
}

func TestRedisBudget_ScriptEnforcesLimitAtomically(t *testing.T) {
	rdb, mr := newRedis(t)
	b := NewBudgetCircuitBreaker(rdb)
	ctx := context.Background()
	limit := decimal.NewFromInt(10)

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := b.EvaluateAndIncrementBudget(ctx, "ws", "agent", decimal.NewFromInt(1), limit)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if r.Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 10 {
		t.Errorf("exactly 10 of 40 requests must fit a $10 limit, got %d", allowed)
	}

	// The key is stored as integer micro-USD and expires.
	keys := mr.Keys()
	if len(keys) != 1 {
		t.Fatalf("expected one budget key, got %v", keys)
	}
	if v, _ := mr.Get(keys[0]); v != "10000000" {
		t.Errorf("expected 10000000 micro-USD, got %s", v)
	}
	if ttl := mr.TTL(keys[0]); ttl <= 0 {
		t.Errorf("budget key must have a TTL, got %v", ttl)
	}
}

func TestRedisBudget_WarningAndBreaker(t *testing.T) {
	rdb, _ := newRedis(t)
	b := NewBudgetCircuitBreaker(rdb)
	ctx := context.Background()
	limit := decimal.NewFromInt(10)

	r, _ := b.EvaluateAndIncrementBudget(ctx, "ws", "a", decimal.RequireFromString("7.9"), limit)
	if !r.Allowed || r.IsWarning80 {
		t.Errorf("7.9/10 should be allowed without warning: %+v", r)
	}
	r, _ = b.EvaluateAndIncrementBudget(ctx, "ws", "a", decimal.RequireFromString("0.1"), limit)
	if !r.Allowed || !r.IsWarning80 {
		t.Errorf("8.0/10 should be allowed with 80%% warning: %+v", r)
	}
	r, _ = b.EvaluateAndIncrementBudget(ctx, "ws", "a", decimal.RequireFromString("2.5"), limit)
	if r.Allowed || !r.IsCircuitBroken || !r.SpentTodayUSD.Equal(decimal.NewFromInt(8)) {
		t.Errorf("over-limit must be denied without recording: %+v", r)
	}
	// Fractional micro-dollar amounts survive the integer encoding.
	r, _ = b.EvaluateAndIncrementBudget(ctx, "ws", "b", decimal.RequireFromString("0.000420"), limit)
	if !r.SpentTodayUSD.Equal(decimal.RequireFromString("0.00042")) {
		t.Errorf("micro-USD precision lost: %s", r.SpentTodayUSD)
	}
}

func TestRedisIdempotency_LockReplayRelease(t *testing.T) {
	rdb, mr := newRedis(t)
	m := NewIdempotencyManager(rdb)
	ctx := context.Background()

	r, err := m.AcquireLock(ctx, "ws", "k", time.Minute)
	if err != nil || !r.Acquired {
		t.Fatalf("first acquire: %+v %v", r, err)
	}
	if r, _ = m.AcquireLock(ctx, "ws", "k", time.Minute); r.Acquired || r.IsReplayed {
		t.Errorf("in-flight duplicate must not acquire or replay: %+v", r)
	}

	if err := m.StoreResult(ctx, "ws", "k", `{"ok":1}`, time.Hour); err != nil {
		t.Fatal(err)
	}
	if mr.Exists("idemp:lock:ws:k") {
		t.Error("lock must be released when the result is stored")
	}
	if r, _ = m.AcquireLock(ctx, "ws", "k", time.Minute); !r.IsReplayed || r.CachedResultJSON != `{"ok":1}` {
		t.Errorf("expected replay: %+v", r)
	}

	_, _ = m.AcquireLock(ctx, "ws", "k2", time.Minute)
	_ = m.ReleaseLock(ctx, "ws", "k2")
	if r, _ = m.AcquireLock(ctx, "ws", "k2", time.Minute); !r.Acquired {
		t.Error("lock should be re-acquirable after release")
	}

	// Lock expires on its own (crashed worker).
	_, _ = m.AcquireLock(ctx, "ws", "k3", 50*time.Millisecond)
	mr.FastForward(time.Second)
	if r, _ = m.AcquireLock(ctx, "ws", "k3", time.Minute); !r.Acquired {
		t.Error("expired lock must be re-acquirable")
	}
}

func TestRedisIdempotency_ConcurrentAcquireOnlyOneWins(t *testing.T) {
	rdb, _ := newRedis(t)
	m := NewIdempotencyManager(rdb)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := m.AcquireLock(context.Background(), "ws", "same", time.Minute)
			if err == nil && r.Acquired {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("exactly one concurrent caller may win the lock, got %d", wins)
	}
}

func TestRedisDown_ReturnsErrorsInsteadOfAllowing(t *testing.T) {
	rdb, mr := newRedis(t)
	mr.Close()
	if _, err := NewBudgetCircuitBreaker(rdb).EvaluateAndIncrementBudget(context.Background(), "ws", "a", decimal.NewFromInt(1), decimal.NewFromInt(10)); err == nil {
		t.Error("budget must error (fail closed) when Redis is down")
	}
	if _, err := NewIdempotencyManager(rdb).AcquireLock(context.Background(), "ws", "k", time.Minute); err == nil {
		t.Error("idempotency must error (fail closed) when Redis is down")
	}
}

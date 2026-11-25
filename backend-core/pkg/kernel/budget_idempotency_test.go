package kernel

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestBudget_EnforcesLimitAndDoesNotRecordDenied(t *testing.T) {
	b := NewBudgetCircuitBreaker(nil)
	ctx := context.Background()
	limit := decimal.NewFromInt(10)

	r, err := b.EvaluateAndIncrementBudget(ctx, "ws", "a1", decimal.NewFromInt(8), limit)
	if err != nil || !r.Allowed || !r.IsWarning80 {
		t.Fatalf("8/10 should be allowed with 80%% warning: %+v err=%v", r, err)
	}
	r, _ = b.EvaluateAndIncrementBudget(ctx, "ws", "a1", decimal.NewFromInt(3), limit)
	if r.Allowed || !r.IsCircuitBroken {
		t.Fatalf("11/10 should be blocked: %+v", r)
	}
	if !r.SpentTodayUSD.Equal(decimal.NewFromInt(8)) {
		t.Errorf("denied spend must not be recorded, got %s", r.SpentTodayUSD)
	}
	// A smaller request that still fits is allowed after a denial.
	if r, _ = b.EvaluateAndIncrementBudget(ctx, "ws", "a1", decimal.NewFromInt(2), limit); !r.Allowed {
		t.Errorf("2 more should fit exactly in the limit: %+v", r)
	}
	// Other agent has its own budget.
	if r, _ = b.EvaluateAndIncrementBudget(ctx, "ws", "a2", decimal.NewFromInt(5), limit); !r.Allowed {
		t.Errorf("separate agent should have separate budget")
	}
}

func TestBudget_ConcurrentNeverExceedsLimit(t *testing.T) {
	b := NewBudgetCircuitBreaker(nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _ := b.EvaluateAndIncrementBudget(context.Background(), "ws", "a", decimal.NewFromInt(1), decimal.NewFromInt(10))
			if r.Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 10 {
		t.Errorf("expected exactly 10 allowed, got %d", allowed)
	}
}

func TestBudget_RejectsNegativeCost(t *testing.T) {
	b := NewBudgetCircuitBreaker(nil)
	if _, err := b.EvaluateAndIncrementBudget(context.Background(), "ws", "a", decimal.NewFromInt(-1), decimal.NewFromInt(10)); err == nil {
		t.Error("negative cost must be rejected (it would refund budget)")
	}
}

func TestIdempotency_LockReplayAndRelease(t *testing.T) {
	m := NewIdempotencyManager(nil)
	ctx := context.Background()

	r, err := m.AcquireLock(ctx, "ws", "k1", time.Minute)
	if err != nil || !r.Acquired {
		t.Fatalf("first acquire should succeed: %+v %v", r, err)
	}
	if r, _ = m.AcquireLock(ctx, "ws", "k1", time.Minute); r.Acquired || r.IsReplayed {
		t.Errorf("second acquire while in-flight must fail without replay: %+v", r)
	}

	if err := m.StoreResult(ctx, "ws", "k1", `{"ok":true}`, time.Hour); err != nil {
		t.Fatal(err)
	}
	if r, _ = m.AcquireLock(ctx, "ws", "k1", time.Minute); !r.IsReplayed || r.CachedResultJSON != `{"ok":true}` {
		t.Errorf("completed key must replay cached result: %+v", r)
	}

	// Failed operations release the lock so they can be retried.
	_, _ = m.AcquireLock(ctx, "ws", "k2", time.Minute)
	_ = m.ReleaseLock(ctx, "ws", "k2")
	if r, _ = m.AcquireLock(ctx, "ws", "k2", time.Minute); !r.Acquired {
		t.Error("lock should be re-acquirable after release")
	}

	// Keys are scoped per workspace.
	if r, _ = m.AcquireLock(ctx, "ws-other", "k1", time.Minute); !r.Acquired {
		t.Error("same key in a different workspace must not collide")
	}
}

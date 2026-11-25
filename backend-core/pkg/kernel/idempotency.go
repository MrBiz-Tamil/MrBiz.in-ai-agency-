package kernel

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

type IdempotencyManager struct {
	redisClient *redis.Client

	// In-process fallback used only when Redis is not configured (single-instance / dev).
	mu      sync.Mutex
	locks   map[string]time.Time
	results map[string]localResult
}

type localResult struct {
	json    string
	expires time.Time
}

type IdempotencyLockResult struct {
	Acquired         bool   `json:"acquired"`
	IsReplayed       bool   `json:"is_replayed"`
	CachedResultJSON string `json:"cached_result_json"`
}

func NewIdempotencyManager(rdb *redis.Client) *IdempotencyManager {
	return &IdempotencyManager{
		redisClient: rdb,
		locks:       make(map[string]time.Time),
		results:     make(map[string]localResult),
	}
}

func idempKeys(workspaceID, idempotencyKey string) (resultKey, lockKey string) {
	return fmt.Sprintf("idemp:res:%s:%s", workspaceID, idempotencyKey),
		fmt.Sprintf("idemp:lock:%s:%s", workspaceID, idempotencyKey)
}

// AcquireLock performs a lock check for the idempotency key. If the operation already
// finished, it returns the cached result with IsReplayed=true.
func (m *IdempotencyManager) AcquireLock(
	ctx context.Context,
	workspaceID string,
	idempotencyKey string,
	ttl time.Duration,
) (IdempotencyLockResult, error) {
	resultKey, lockKey := idempKeys(workspaceID, idempotencyKey)

	if m.redisClient == nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		now := time.Now()
		if r, ok := m.results[resultKey]; ok && now.Before(r.expires) {
			return IdempotencyLockResult{IsReplayed: true, CachedResultJSON: r.json}, nil
		}
		if exp, ok := m.locks[lockKey]; ok && now.Before(exp) {
			return IdempotencyLockResult{}, nil
		}
		m.locks[lockKey] = now.Add(ttl)
		return IdempotencyLockResult{Acquired: true}, nil
	}

	// 1. Check if previously completed
	cachedResult, err := m.redisClient.Get(ctx, resultKey).Result()
	if err == nil && cachedResult != "" {
		return IdempotencyLockResult{
			Acquired:         false,
			IsReplayed:       true,
			CachedResultJSON: cachedResult,
		}, nil
	}
	if err != nil && err != redis.Nil {
		return IdempotencyLockResult{}, err
	}

	// 2. Attempt to acquire atomic execution lock
	acquired, err := m.redisClient.SetNX(ctx, lockKey, "locked", ttl).Result()
	if err != nil {
		return IdempotencyLockResult{}, err
	}
	return IdempotencyLockResult{Acquired: acquired}, nil
}

// StoreResult caches the execution result for deterministic replay and releases the lock.
func (m *IdempotencyManager) StoreResult(
	ctx context.Context,
	workspaceID string,
	idempotencyKey string,
	resultJSON string,
	retention time.Duration,
) error {
	resultKey, lockKey := idempKeys(workspaceID, idempotencyKey)

	if m.redisClient == nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.results[resultKey] = localResult{json: resultJSON, expires: time.Now().Add(retention)}
		delete(m.locks, lockKey)
		return nil
	}

	pipe := m.redisClient.TxPipeline()
	pipe.Set(ctx, resultKey, resultJSON, retention)
	pipe.Del(ctx, lockKey)
	_, err := pipe.Exec(ctx)
	return err
}

// ReleaseLock frees the execution lock if an operation failed and should be retried.
func (m *IdempotencyManager) ReleaseLock(
	ctx context.Context,
	workspaceID string,
	idempotencyKey string,
) error {
	_, lockKey := idempKeys(workspaceID, idempotencyKey)
	if m.redisClient == nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.locks, lockKey)
		return nil
	}
	return m.redisClient.Del(ctx, lockKey).Err()
}

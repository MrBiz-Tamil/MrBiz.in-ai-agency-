package ratelimit_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/mrbiz/venture-os/internal/ratelimit"
)

func exercise(t *testing.T, l ratelimit.Limiter, advance func(time.Duration)) {
	t.Helper()
	ctx := context.Background()
	a, b := uuid.New(), uuid.New()
	for i := 0; i < 3; i++ {
		if ok, _, err := l.Allow(ctx, a); err != nil || !ok {
			t.Fatalf("call %d: ok=%v err=%v", i+1, ok, err)
		}
	}
	ok, retry, err := l.Allow(ctx, a)
	if err != nil || ok || retry <= 0 || retry > time.Minute {
		t.Fatalf("4th call: ok=%v retry=%v err=%v", ok, retry, err)
	}
	// Another tenant has its own budget.
	if ok, _, _ := l.Allow(ctx, b); !ok {
		t.Fatal("tenant b was throttled by tenant a's traffic")
	}
	// The window resets.
	advance(time.Minute + time.Second)
	if ok, _, _ := l.Allow(ctx, a); !ok {
		t.Fatal("limit did not reset in the next window")
	}
}

func TestMemory(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	l := &ratelimit.Memory{Limit: 3, Window: time.Minute, Now: func() time.Time { return now }}
	exercise(t, l, func(d time.Duration) { now = now.Add(d) })
}

func TestRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	l := &ratelimit.Redis{Client: client, Limit: 3, Window: time.Minute, Now: func() time.Time { return now }}
	tenant := uuid.New()
	exercise(t, l, func(d time.Duration) { now = now.Add(d) })

	// Keys are tenant-prefixed and expire by themselves.
	_, _, _ = l.Allow(context.Background(), tenant)
	keys := mr.Keys()
	if len(keys) == 0 {
		t.Fatal("no keys written")
	}
	for _, k := range keys {
		if len(k) < 2 || k[:2] != "t:" {
			t.Errorf("key %q lacks the t:{tenant}: prefix", k)
		}
		if mr.TTL(k) <= 0 {
			t.Errorf("key %q has no expiry", k)
		}
	}
}

func TestRedisReportsErrorsAndRejectsBadConfig(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	if _, _, err := (&ratelimit.Redis{Client: client}).Allow(context.Background(), uuid.New()); err == nil {
		t.Error("zero limit/window accepted")
	}
	mr.Close()
	l := &ratelimit.Redis{Client: client, Limit: 1, Window: time.Minute}
	if _, _, err := l.Allow(context.Background(), uuid.New()); err == nil {
		t.Error("redis outage must surface as an error so the caller can decide")
	}
}

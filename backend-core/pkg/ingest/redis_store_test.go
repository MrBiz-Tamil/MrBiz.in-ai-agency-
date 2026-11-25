package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestRedisEventStore_EndToEnd(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	svc := NewIngestService(NewRedisEventStore(rdb, "stream:test"))
	body := []byte(`{"id":77}`)

	rec := post(t, svc, body, sign(body, "s3cret"), map[string]string{"X-External-ID": "77", "X-Event-Type": "order.created"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	entries, err := rdb.XRange(context.Background(), "stream:test", "-", "+").Result()
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one stream entry, got %d err=%v", len(entries), err)
	}
	var ev WebhookEvent
	if err := json.Unmarshal([]byte(entries[0].Values["event"].(string)), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.WorkspaceID != testWorkspace || ev.ExternalID != "77" || string(ev.Payload) != string(body) {
		t.Errorf("unexpected event: %+v", ev)
	}

	// Duplicate: acknowledged, not re-enqueued.
	rec = post(t, svc, body, sign(body, "s3cret"), map[string]string{"X-External-ID": "77"})
	if rec.Header().Get("X-Dedup-Status") != "REPLAY_ACK" {
		t.Errorf("expected REPLAY_ACK, got %q", rec.Header().Get("X-Dedup-Status"))
	}
	if n, _ := rdb.XLen(context.Background(), "stream:test").Result(); n != 1 {
		t.Errorf("duplicate must not be re-enqueued, stream length %d", n)
	}

	// The dedup key expires after 24h.
	mr.FastForward(25 * time.Hour)
	rec = post(t, svc, body, sign(body, "s3cret"), map[string]string{"X-External-ID": "77"})
	if rec.Header().Get("X-Dedup-Status") == "REPLAY_ACK" {
		t.Error("dedup key should have expired after 24h")
	}
}

func TestRedisEventStore_RedisDownReturns503(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	svc := NewIngestService(NewRedisEventStore(rdb, "s"))
	mr.Close()
	body := []byte(`{"id":1}`)
	if rec := post(t, svc, body, sign(body, "s3cret"), nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("Redis outage must return 503 so the sender retries, got %d", rec.Code)
	}
}

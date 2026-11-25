package ingest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeStore struct {
	mu         sync.Mutex
	claimed    map[string]bool
	events     []WebhookEvent
	claimErr   error
	enqueueErr error
}

func newFakeStore() *fakeStore { return &fakeStore{claimed: map[string]bool{}} }

func (f *fakeStore) ClaimDedup(_ context.Context, key string, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return false, f.claimErr
	}
	if f.claimed[key] {
		return false, nil
	}
	f.claimed[key] = true
	return true, nil
}

func (f *fakeStore) ReleaseDedup(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.claimed, key)
	return nil
}

func (f *fakeStore) Enqueue(_ context.Context, ev WebhookEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.enqueueErr != nil {
		return f.enqueueErr
	}
	f.events = append(f.events, ev)
	return nil
}

const testWorkspace = "6f1c2c1e-1b0a-4d7e-9a34-0c2a5f2b7e11"

func testConfig() SourceConfig {
	return SourceConfig{Source: "shopify", Secret: "s3cret", WorkspaceID: testWorkspace, Base64Sig: true}
}

func sign(body []byte, secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func post(t *testing.T, svc *IngestService, body []byte, sig string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/shopify", bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	svc.HandleIncomingWebhook(rec, req, testConfig(), sig)
	return rec
}

func TestWebhook_AcceptsAndDedups(t *testing.T) {
	store := newFakeStore()
	svc := NewIngestService(store)
	body := []byte(`{"id":1}`)

	// X-Workspace-ID must be ignored: the workspace is bound to the configured secret.
	rec := post(t, svc, body, sign(body, "s3cret"), map[string]string{"X-Workspace-ID": "evil"})
	if rec.Code != http.StatusOK || len(store.events) != 1 {
		t.Fatalf("expected accepted+enqueued, got %d events=%d", rec.Code, len(store.events))
	}
	if store.events[0].WorkspaceID != testWorkspace {
		t.Errorf("workspace must come from config, got %q", store.events[0].WorkspaceID)
	}

	rec = post(t, svc, body, sign(body, "s3cret"), nil)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Dedup-Status") != "REPLAY_ACK" || len(store.events) != 1 {
		t.Errorf("expected duplicate ack without re-enqueue, got %d", rec.Code)
	}
}

func TestWebhook_RejectsBadSignature(t *testing.T) {
	svc := NewIngestService(newFakeStore())
	body := []byte(`{"id":1}`)
	if rec := post(t, svc, body, sign(body, "wrong"), nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestWebhook_EnqueueFailureReturns503AndAllowsRetry(t *testing.T) {
	store := newFakeStore()
	store.enqueueErr = errors.New("redis down")
	svc := NewIngestService(store)
	body := []byte(`{"id":2}`)

	if rec := post(t, svc, body, sign(body, "s3cret"), nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on enqueue failure, got %d", rec.Code)
	}

	// Sender retries once Redis is back: the event must NOT be dropped as a duplicate.
	store.enqueueErr = nil
	if rec := post(t, svc, body, sign(body, "s3cret"), nil); rec.Code != http.StatusOK || len(store.events) != 1 {
		t.Errorf("retry should enqueue, got %d events=%d", rec.Code, len(store.events))
	}
}

func TestWebhook_StoreUnavailable(t *testing.T) {
	body := []byte(`{"id":3}`)
	if rec := post(t, NewIngestService(nil), body, sign(body, "s3cret"), nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nil store must return 503, got %d", rec.Code)
	}
	store := newFakeStore()
	store.claimErr = errors.New("redis down")
	if rec := post(t, NewIngestService(store), body, sign(body, "s3cret"), nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("dedup failure must return 503, got %d", rec.Code)
	}
}

func TestWebhook_OversizeBodyRejected(t *testing.T) {
	svc := NewIngestService(newFakeStore())
	body := []byte(strings.Repeat("a", maxBodyBytes+10))
	if rec := post(t, svc, body, sign(body, "s3cret"), nil); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", rec.Code)
	}
}

func TestSourceConfigValidate(t *testing.T) {
	if err := (SourceConfig{Source: "shopify", Secret: "x", WorkspaceID: "ws_default"}).Validate(); err == nil {
		t.Error("non-UUID workspace must be rejected")
	}
	if err := (SourceConfig{Source: "shopify", Secret: "", WorkspaceID: testWorkspace}).Validate(); err == nil {
		t.Error("empty secret must be rejected")
	}
	if err := testConfig().Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

func TestWebhook_LatencyHeaderIsNumericMilliseconds(t *testing.T) {
	svc := NewIngestService(newFakeStore())
	body := []byte(`{"id":9}`)
	rec := post(t, svc, body, sign(body, "s3cret"), nil)
	if _, err := strconv.Atoi(rec.Header().Get("X-Ingest-Latency-Ms")); err != nil {
		t.Errorf("X-Ingest-Latency-Ms must be an integer number of milliseconds, got %q", rec.Header().Get("X-Ingest-Latency-Ms"))
	}
}

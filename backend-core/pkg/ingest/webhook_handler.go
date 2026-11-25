package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const maxBodyBytes = 5 * 1024 * 1024

type WebhookEvent struct {
	Source         string          `json:"source"`
	WorkspaceID    string          `json:"workspace_id"`
	EventType      string          `json:"event_type"`
	ExternalID     string          `json:"external_id"`
	DedupKey       string          `json:"dedup_key"`
	Payload        json.RawMessage `json:"payload"`
	ReceivedAtUnix int64           `json:"received_at_unix"`
}

// EventStore is the durable backend used for webhook de-duplication and queueing.
type EventStore interface {
	// ClaimDedup reserves the dedup key; isNew is false if it was already claimed.
	ClaimDedup(ctx context.Context, key string, ttl time.Duration) (isNew bool, err error)
	// ReleaseDedup removes a previously claimed dedup key (used when enqueueing fails).
	ReleaseDedup(ctx context.Context, key string) error
	// Enqueue appends the event to the processing stream.
	Enqueue(ctx context.Context, ev WebhookEvent) error
}

type redisEventStore struct {
	rdb    *redis.Client
	stream string
}

// NewRedisEventStore returns an EventStore backed by a Redis key (dedup) and stream (queue).
func NewRedisEventStore(rdb *redis.Client, stream string) EventStore {
	if stream == "" {
		stream = "stream:webhooks:incoming"
	}
	return &redisEventStore{rdb: rdb, stream: stream}
}

func (s *redisEventStore) ClaimDedup(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return s.rdb.SetNX(ctx, "dedup:webhook:"+key, "1", ttl).Result()
}

func (s *redisEventStore) ReleaseDedup(ctx context.Context, key string) error {
	return s.rdb.Del(ctx, "dedup:webhook:"+key).Err()
}

func (s *redisEventStore) Enqueue(ctx context.Context, ev WebhookEvent) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return s.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: s.stream,
		// Bound the stream so an unconsumed queue cannot grow without limit.
		MaxLen: 100000,
		Approx: true,
		Values: map[string]interface{}{"event": string(b)},
	}).Err()
}

// SourceConfig holds the trusted, server-side configuration for one webhook source.
// The workspace is bound to the secret, never taken from request headers.
type SourceConfig struct {
	Source      string
	Secret      string
	WorkspaceID string // must be a UUID
	Base64Sig   bool   // true: base64 signature (Shopify); false: hex
}

func (c SourceConfig) Validate() error {
	if c.Source == "" {
		return errors.New("webhook source name is required")
	}
	if c.Secret == "" {
		return ErrEmptySecret
	}
	if _, err := uuid.Parse(c.WorkspaceID); err != nil {
		return errors.New("webhook workspace id must be a UUID")
	}
	return nil
}

type IngestService struct {
	store EventStore
}

func NewIngestService(store EventStore) *IngestService {
	return &IngestService{store: store}
}

// Handler returns an http.Handler for a single configured webhook source. The signature
// header name is supplied by the caller (e.g. "X-Shopify-Hmac-Sha256").
func (s *IngestService) Handler(cfg SourceConfig, signatureHeaderName string) (http.Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.HandleIncomingWebhook(w, r, cfg, r.Header.Get(signatureHeaderName))
	}), nil
}

// HandleIncomingWebhook verifies the HMAC, de-duplicates and durably enqueues the event.
// It only returns 2xx once the event is safely queued (or is a confirmed duplicate); on any
// storage failure it returns 503 so the sender retries.
func (s *IngestService) HandleIncomingWebhook(
	w http.ResponseWriter,
	r *http.Request,
	cfg SourceConfig,
	signatureHeader string,
) {
	start := time.Now()
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	// Read up to maxBodyBytes+1 so oversize bodies are rejected instead of silently truncated.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	if len(body) > maxBodyBytes {
		http.Error(w, "Payload too large", http.StatusRequestEntityTooLarge)
		return
	}

	var valid bool
	if cfg.Base64Sig {
		valid = VerifyHMACSHA256Base64(body, cfg.Secret, signatureHeader)
	} else {
		valid = VerifyHMACSHA256Hex(body, cfg.Secret, signatureHeader)
	}
	if !valid {
		http.Error(w, "Unauthorized: invalid signature", http.StatusUnauthorized)
		return
	}

	if s.store == nil {
		http.Error(w, "Event queue unavailable", http.StatusServiceUnavailable)
		return
	}

	externalID := r.Header.Get("X-External-ID")
	eventType := r.Header.Get("X-Event-Type")
	if eventType == "" {
		eventType = "order.created"
	}
	dedupKey := ComputePayloadDeduplicationKey(cfg.Source, externalID, body)

	ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
	defer cancel()

	isNew, err := s.store.ClaimDedup(ctx, dedupKey, 24*time.Hour)
	if err != nil {
		http.Error(w, "Deduplication store unavailable", http.StatusServiceUnavailable)
		return
	}
	if !isNew {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Dedup-Status", "REPLAY_ACK")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"already_acknowledged"}`))
		return
	}

	event := WebhookEvent{
		Source:         cfg.Source,
		WorkspaceID:    cfg.WorkspaceID,
		EventType:      eventType,
		ExternalID:     externalID,
		DedupKey:       dedupKey,
		Payload:        body,
		ReceivedAtUnix: time.Now().Unix(),
	}
	if err := s.store.Enqueue(ctx, event); err != nil {
		// Free the dedup key so the sender's retry is processed instead of being dropped.
		relCtx, relCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer relCancel()
		_ = s.store.ReleaseDedup(relCtx, dedupKey)
		http.Error(w, "Failed to enqueue event", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Ingest-Latency-Ms", strconv.FormatInt(time.Since(start).Milliseconds(), 10))
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted", "dedup_key": dedupKey})
}

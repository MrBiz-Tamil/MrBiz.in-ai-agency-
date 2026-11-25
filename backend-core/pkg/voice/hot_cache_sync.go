package voice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
)

var ErrInvalidWorkspace = errors.New("workspace id must be a UUID")

type HotOrderState struct {
	OrderID       string          `json:"order_id"`
	CustomerName  string          `json:"customer_name"`
	CustomerPhone string          `json:"customer_phone"` // E.164; the voice agent only discloses the order to this caller
	Status        string          `json:"status"`
	Carrier       string          `json:"carrier"`
	TrackingNum   string          `json:"tracking_num"`
	ETA           string          `json:"eta"`
	AmountINR     decimal.Decimal `json:"amount_inr"`
}

type HotSKUState struct {
	SKU      string          `json:"sku"`
	Name     string          `json:"name"`
	InStock  int32           `json:"in_stock"`
	PriceINR decimal.Decimal `json:"price_inr"`
}

type HotCacheSynchronizer struct {
	redisClient *redis.Client
}

func NewHotCacheSynchronizer(rdb *redis.Client) *HotCacheSynchronizer {
	return &HotCacheSynchronizer{redisClient: rdb}
}

// SyncOrderState updates Redis with sub-5ms lookup state for the voice Talker agent.
func (s *HotCacheSynchronizer) SyncOrderState(
	ctx context.Context,
	workspaceID string,
	order HotOrderState,
	ttl time.Duration,
) error {
	if _, err := uuid.Parse(workspaceID); err != nil {
		return ErrInvalidWorkspace
	}
	if s.redisClient == nil {
		return nil
	}
	key := fmt.Sprintf("order:%s:%s", workspaceID, order.OrderID)
	data, err := json.Marshal(order)
	if err != nil {
		return err
	}
	return s.redisClient.Set(ctx, key, string(data), ttl).Err()
}

// SyncSKUState updates live inventory buffers for the voice Talker agent.
func (s *HotCacheSynchronizer) SyncSKUState(
	ctx context.Context,
	workspaceID string,
	sku HotSKUState,
	ttl time.Duration,
) error {
	if _, err := uuid.Parse(workspaceID); err != nil {
		return ErrInvalidWorkspace
	}
	if s.redisClient == nil {
		return nil
	}
	key := fmt.Sprintf("sku:%s:%s", workspaceID, sku.SKU)
	data, err := json.Marshal(sku)
	if err != nil {
		return err
	}
	return s.redisClient.Set(ctx, key, string(data), ttl).Err()
}

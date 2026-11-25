package voice

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

const testWS = "6f1c2c1e-1b0a-4d7e-9a34-0c2a5f2b7e11"

func TestHotCacheSynchronizer_KeyGeneration(t *testing.T) {
	syncer := NewHotCacheSynchronizer(nil) // nil client tests validation logic

	order := HotOrderState{
		OrderID:       "ord_9901",
		CustomerName:  "Kavita Rao",
		CustomerPhone: "+919800000001",
		Status:        "OUT_FOR_DELIVERY",
		Carrier:       "Delhivery",
		TrackingNum:   "DL-109281",
		ETA:           "Today by 3 PM",
		AmountINR:     decimal.RequireFromString("2450.00"),
	}
	if err := syncer.SyncOrderState(context.Background(), testWS, order, 24*time.Hour); err != nil {
		t.Errorf("unexpected error syncing order: %v", err)
	}

	sku := HotSKUState{SKU: "SKU-SHIRT-M", Name: "Cotton Shirt", InStock: 45, PriceINR: decimal.RequireFromString("1299.00")}
	if err := syncer.SyncSKUState(context.Background(), testWS, sku, 24*time.Hour); err != nil {
		t.Errorf("unexpected error syncing sku: %v", err)
	}
}

func TestHotCacheSynchronizer_RejectsNonUUIDWorkspace(t *testing.T) {
	syncer := NewHotCacheSynchronizer(nil)
	err := syncer.SyncOrderState(context.Background(), "ws_mrbiz", HotOrderState{OrderID: "o"}, time.Hour)
	if !errors.Is(err, ErrInvalidWorkspace) {
		t.Errorf("expected ErrInvalidWorkspace, got %v", err)
	}
	err = syncer.SyncSKUState(context.Background(), "ws_mrbiz", HotSKUState{SKU: "s"}, time.Hour)
	if !errors.Is(err, ErrInvalidWorkspace) {
		t.Errorf("expected ErrInvalidWorkspace, got %v", err)
	}
}

package provisioner_test

// Regression tests for a defect found by the 2026-11-25 audit of the provisioning saga. An idempotency key names ONE
// command: reusing it for a different blueprint used to return the earlier run's stored outcome. Each failed before the fix.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/provisioner"
)

func TestKeyReusedForADifferentBlueprintIsRefusedAfterASuccess(t *testing.T) {
	w := newWorld(t)
	first := w.request("shared-key")
	if _, err := w.p.Provision(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	calls := w.hub.ExternalCalls()

	second := first
	second.BlueprintID = uuid.New() // same tenant, same key, a different blueprint
	res, err := w.p.Provision(context.Background(), second)
	if !errors.Is(err, provisioner.ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest, got result=%+v err=%v", res, err)
	}
	if w.hub.ExternalCalls() != calls {
		t.Fatal("a refused request touched external systems")
	}
}

func TestKeyReusedForADifferentBlueprintIsRefusedAfterAFailure(t *testing.T) {
	w := newWorld(t)
	first := w.request("shared-key")
	w.hub.Fail("crm", "EnsureWorkspace", errors.New("twenty: 400"), 1)
	if _, err := w.p.Provision(context.Background(), first); err == nil {
		t.Fatal("want failure")
	}

	// The stored failure belongs to the first blueprint; the new one must not be told it failed, nor be skipped.
	second := first
	second.BlueprintID = uuid.New()
	_, err := w.p.Provision(context.Background(), second)
	var pe *provisioner.Error
	if errors.As(err, &pe) || !errors.Is(err, provisioner.ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest (not the old run's failure), got: %v", err)
	}

	// With a fresh key the new blueprint provisions normally.
	second.IdempotencyKey = "fresh-key"
	if _, err := w.p.Provision(context.Background(), second); err != nil {
		t.Fatalf("a fresh key must work: %v", err)
	}
}

func TestSameKeyAndSameBlueprintStillReplays(t *testing.T) {
	w := newWorld(t)
	req := w.request("k")
	first, err := w.p.Provision(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := w.p.Provision(context.Background(), req)
	if err != nil || !again.Replayed || again.RunID != first.RunID {
		t.Fatalf("a true replay must still work: %+v %v", again, err)
	}
}

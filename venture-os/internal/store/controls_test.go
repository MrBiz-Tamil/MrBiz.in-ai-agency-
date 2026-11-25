package store_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/store"
	"github.com/mrbiz/venture-os/internal/vault"
)

func i64(v int64) *int64 { return &v }

func queue(t *testing.T, s *store.PG, tn uuid.UUID, kind string, minor int64, cur, key string) store.Approval {
	t.Helper()
	a, _, err := s.CreateApproval(ctx, tn, store.NewApproval{Kind: kind, Title: key, RequestedByType: "AGENT", RequestedBy: "agent:test", Risk: "HIGH",
		Payload: json.RawMessage(`{"k":"` + key + `"}`), AmountMinor: &minor, Currency: cur, IdempotencyKey: key})
	must(t, err)
	return a
}

func TestSpendCapsBlockApprovalsThatWouldBreakThem(t *testing.T) {
	s, pool := newStore(t)
	tn, _ := approvedTenant(t, s, "founder")
	cap := func(c store.SpendCap) {
		c.UpdatedBy = "founder"
		must(t, s.SetSpendCap(ctx, tn, c))
	}
	approve := func(a store.Approval) error {
		_, err := s.DecideApproval(ctx, tn, a.ID, true, "founder", "", a.PayloadHash)
		return err
	}
	status := func(a store.Approval) string {
		got, err := s.Approval(ctx, tn, a.ID)
		must(t, err)
		return got.Status
	}

	t.Run("per transaction", func(t *testing.T) {
		cap(store.SpendCap{Currency: "INR", PerTransaction: i64(50_000)})
		big := queue(t, s, tn, "PAYMENT", 100_000, "INR", "pay-big")
		err := approve(big)
		var sc *store.SpendCapError
		if !errors.Is(err, store.ErrSpendCap) || !errors.As(err, &sc) || sc.Dimension != "per transaction" || sc.Cap != 50_000 {
			t.Fatalf("want a per-transaction cap error, got %v", err)
		}
		if status(big) != "PENDING_APPROVAL" {
			t.Fatal("a blocked approval must stay pending: nothing was approved")
		}
		small := queue(t, s, tn, "PAYMENT", 50_000, "INR", "pay-exact")
		if err := approve(small); err != nil {
			t.Fatalf("an amount equal to the cap is allowed: %v", err)
		}
	})

	t.Run("rejecting is never blocked, and other currencies and kinds are not capped", func(t *testing.T) {
		cap(store.SpendCap{Currency: "INR", PerTransaction: i64(1)})
		over := queue(t, s, tn, "PAYMENT", 9_999, "INR", "pay-reject")
		if _, err := s.DecideApproval(ctx, tn, over.ID, false, "founder", "no", [32]byte{}); err != nil {
			t.Fatalf("a rejection must not be blocked by a cap: %v", err)
		}
		if err := approve(queue(t, s, tn, "PAYMENT", 9_999, "USD", "pay-usd")); err != nil {
			t.Fatalf("an INR cap must not limit USD: %v", err)
		}
		if err := approve(queue(t, s, tn, "INVOICE", 9_999_999, "INR", "inv-1")); err != nil {
			t.Fatalf("money coming in is not capped: %v", err)
		}
	})

	t.Run("per hour and per day windows", func(t *testing.T) {
		cap(store.SpendCap{Currency: "EUR", PerHour: i64(150_000), PerDay: i64(250_000)})
		first := queue(t, s, tn, "PURCHASE_ORDER", 100_000, "EUR", "po-1")
		must(t, approve(first))
		second := queue(t, s, tn, "PURCHASE_ORDER", 100_000, "EUR", "po-2")
		var sc *store.SpendCapError
		if err := approve(second); !errors.As(err, &sc) || sc.Dimension != "in the last hour" || sc.Spent != 100_000 {
			t.Fatalf("second purchase order within the hour: %v", err)
		}
		// Two hours later the hourly window has cleared but the daily one has not.
		if _, err := pool.Exec(ctx, `UPDATE approval_queue SET decided_at = now() - interval '2 hours' WHERE id = $1`, first.ID); err != nil {
			t.Fatal(err)
		}
		must(t, approve(second))
		if _, err := pool.Exec(ctx, `UPDATE approval_queue SET decided_at = now() - interval '2 hours' WHERE id = $1`, second.ID); err != nil {
			t.Fatal(err)
		}
		third := queue(t, s, tn, "PURCHASE_ORDER", 100_000, "EUR", "po-3")
		if err := approve(third); !errors.As(err, &sc) || sc.Dimension != "in the last 24 hours" || sc.Spent != 200_000 {
			t.Fatalf("third purchase order within the day (200,000 already approved, cap 250,000): %v", err)
		}
		// After a day it clears entirely.
		if _, err := pool.Exec(ctx, `UPDATE approval_queue SET decided_at = now() - interval '30 hours' WHERE tenant_id = $1 AND currency = 'EUR'`, tn); err != nil {
			t.Fatal(err)
		}
		must(t, approve(third))
	})

	t.Run("spending exactly up to a window cap is allowed, one unit more is not, and rejected requests are not spend", func(t *testing.T) {
		cap(store.SpendCap{Currency: "CHF", PerHour: i64(200)})
		rejected := queue(t, s, tn, "PAYMENT", 150, "CHF", "chf-rejected")
		if _, err := s.DecideApproval(ctx, tn, rejected.ID, false, "founder", "no", [32]byte{}); err != nil {
			t.Fatal(err)
		}
		must(t, approve(queue(t, s, tn, "PAYMENT", 100, "CHF", "chf-1")))
		must(t, approve(queue(t, s, tn, "PAYMENT", 100, "CHF", "chf-2"))) // 100 + 100 == 200: at the cap, not over it
		if err := approve(queue(t, s, tn, "PAYMENT", 1, "CHF", "chf-3")); !errors.Is(err, store.ErrSpendCap) {
			t.Fatalf("one unit over the cap: %v", err)
		}
	})

	t.Run("removing a cap lifts it, and bad caps are refused", func(t *testing.T) {
		cap(store.SpendCap{Currency: "GBP", PerTransaction: i64(1)})
		cap(store.SpendCap{Currency: "GBP"}) // all nil: remove
		must(t, approve(queue(t, s, tn, "PAYMENT", 5_000_000, "GBP", "pay-gbp")))
		err := s.SetSpendCap(ctx, tn, store.SpendCap{Currency: "JPY", PerHour: i64(10), PerDay: i64(5), UpdatedBy: "f"})
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("an hourly cap above the daily cap is nonsense: %v", err)
		}
	})
}

// With caps in place, many approvals racing must not slip past them together. This only proves something on a real
// multi-connection PostgreSQL (VENTURE_TEST_DATABASE_URL); PGlite serialises connections, so it passes trivially there.
func TestConcurrentApprovalsCannotSlipPastACapTogether(t *testing.T) {
	s, _ := newStore(t)
	tn, _ := approvedTenant(t, s, "founder")
	must(t, s.SetSpendCap(ctx, tn, store.SpendCap{Currency: "INR", PerDay: i64(250), UpdatedBy: "f"}))
	var as []store.Approval
	for i := 0; i < 8; i++ {
		as = append(as, queue(t, s, tn, "PAYMENT", 100, "INR", fmt.Sprintf("race-%d", i)))
	}
	var ok, blocked atomic.Int32
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for _, a := range as {
		wg.Add(1)
		go func(a store.Approval) {
			defer wg.Done()
			<-gate
			_, err := s.DecideApproval(ctx, tn, a.ID, true, "founder", "", a.PayloadHash)
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, store.ErrSpendCap):
				blocked.Add(1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		}(a)
	}
	close(gate)
	wg.Wait()
	if ok.Load() != 2 || blocked.Load() != 6 {
		t.Fatalf("%d approved and %d blocked; a cap of 250 fits exactly two payments of 100", ok.Load(), blocked.Load())
	}
}

func TestConnectionsKeepSecretsSealedAndPerTenant(t *testing.T) {
	s, pool := newStore(t)
	a, _ := approvedTenant(t, s, "founder-a")
	b, _, err := s.CreateTenant(ctx, "founder-b", "create-founder-b") // another business, no blueprint needed
	must(t, err)
	v, _ := vault.New(map[int][]byte{1: bytes.Repeat([]byte{7}, 32)}, 1)
	secret := []byte(`{"refresh_token":"1//super-secret-token"}`)
	sealed, err := v.Seal(a, "google", secret)
	must(t, err)
	must(t, s.SaveConnection(ctx, a, store.Connection{Provider: "google", Config: map[string]string{"calendar_id": "primary"}, SecretName: "google", ConnectedBy: "founder-a"}, sealed))

	c, got, err := s.Connection(ctx, a, "google")
	must(t, err)
	if c.Config["calendar_id"] != "primary" || c.Status != "ACTIVE" || c.ConnectedBy != "founder-a" {
		t.Fatalf("connection = %+v", c)
	}
	if pt, err := v.Open(a, "google", got); err != nil || !bytes.Equal(pt, secret) {
		t.Fatalf("the secret must survive the database: %q %v", pt, err)
	}
	// The plaintext appears nowhere in the table.
	var leaked int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM tenant_secrets WHERE position('super-secret'::bytea in ciphertext) > 0`).Scan(&leaked))
	if leaked != 0 {
		t.Fatal("a secret is stored in the clear")
	}
	if list, _ := s.Connections(ctx, a); len(list) != 1 || list[0].SecretName != "google" {
		t.Fatalf("list = %+v", list)
	}
	// Another business sees nothing, and cannot decrypt a stolen row even with the vault.
	if _, _, err := s.Connection(ctx, b, "google"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("tenant B reads tenant A's connection: %v", err)
	}
	if _, err := v.Open(b, "google", got); err == nil {
		t.Fatal("a ciphertext opened under another tenant")
	}
	// Reconnecting replaces; disconnecting destroys the secret.
	sealed2, _ := v.Seal(a, "google", []byte(`{"refresh_token":"new"}`))
	must(t, s.SaveConnection(ctx, a, store.Connection{Provider: "google", Config: map[string]string{"calendar_id": "team"}, SecretName: "google", ConnectedBy: "founder-a"}, sealed2))
	if c, _, _ := s.Connection(ctx, a, "google"); c.Config["calendar_id"] != "team" {
		t.Fatal("reconnect did not replace the settings")
	}
	must(t, s.DeleteConnection(ctx, a, "google"))
	var n int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM tenant_secrets WHERE tenant_id = $1`, a).Scan(&n))
	if n != 0 {
		t.Fatalf("%d secrets remain after disconnecting", n)
	}
	if err := s.DeleteConnection(ctx, a, "google"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleting twice: %v", err)
	}
}

func TestAuditChainVerifiesThroughTheStore(t *testing.T) {
	s, _ := newStore(t)
	tn, _ := approvedTenant(t, s, "founder")
	for i := 0; i < 4; i++ {
		must(t, s.AuditUser(ctx, tn, "founder", fmt.Sprintf("test.%d", i), "x", "1", map[string]any{"i": i}))
	}
	v, err := s.VerifyAudit(ctx, tn)
	if err != nil || !v.Intact || v.RowsChecked < 4 || len(v.Head) != 32 {
		t.Fatalf("verdict = %+v %v", v, err)
	}
	must(t, s.AuditUser(ctx, tn, "founder", "test.more", "x", "1", nil)) // nil data must be stored as {}, not refused
	v2, _ := s.VerifyAudit(ctx, tn)
	if !v2.Intact || v2.RowsChecked != v.RowsChecked+1 || bytes.Equal(v.Head, v2.Head) {
		t.Fatalf("appending must extend the chain and move the head: %+v -> %+v", v, v2)
	}
}

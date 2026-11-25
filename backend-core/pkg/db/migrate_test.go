package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mrbiz/abos/backend-core/pkg/db"
	"github.com/mrbiz/abos/backend-core/pkg/db/dbtest"
)

func TestMigrations_UpIsIdempotentAndDownReverts(t *testing.T) {
	pool := dbtest.AdminPool(t) // applies all up migrations
	ctx := context.Background()

	ran, err := db.Up(ctx, pool)
	if err != nil || len(ran) != 0 {
		t.Fatalf("second Up must be a no-op, ran=%v err=%v", ran, err)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN
		('workspaces','execution_tickets','audit_xray_logs','business_goals','goal_milestones','policy_rules','users','sessions')`).Scan(&n); err != nil || n != 8 {
		t.Fatalf("expected all 8 tables, got %d err=%v", n, err)
	}

	// Revert EVERY migration (newest first): each down script must be complete, and the schema
	// must be empty afterwards. Then re-apply everything from scratch.
	reverted, err := db.Down(ctx, pool, 100)
	if err != nil || len(reverted) != 4 || !strings.HasPrefix(reverted[0], "000004") || !strings.HasPrefix(reverted[3], "000001") {
		t.Fatalf("down all: %v %v", reverted, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name <> 'schema_migrations'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("reverting every migration must leave no tables behind, %d remain (err=%v)", n, err)
	}
	if _, err := db.Up(ctx, pool); err != nil {
		t.Fatalf("re-apply after down: %v", err)
	}
}

func TestAuditLogIsAppendOnly(t *testing.T) {
	pool := dbtest.AdminPool(t)
	ctx := context.Background()

	var wsID, ticketID string
	if err := pool.QueryRow(ctx, `INSERT INTO workspaces(name) VALUES ('t') RETURNING id`).Scan(&wsID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO execution_tickets(workspace_id, idempotency_key, action_type, payload)
		VALUES ($1,'k','x','{}') RETURNING id`, wsID).Scan(&ticketID); err != nil {
		t.Fatal(err)
	}
	var logID int64
	if err := pool.QueryRow(ctx, `INSERT INTO audit_xray_logs(workspace_id, ticket_id, step_number) VALUES ($1,$2,1) RETURNING id`, wsID, ticketID).Scan(&logID); err != nil {
		t.Fatalf("insert must work: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE audit_xray_logs SET policy_decision='ALLOW' WHERE id=$1`, logID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Errorf("UPDATE must be blocked by the append-only trigger, got %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM audit_xray_logs WHERE id=$1`, logID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Errorf("DELETE must be blocked by the append-only trigger, got %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE audit_xray_logs`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Errorf("TRUNCATE must be blocked by the append-only trigger, got %v", err)
	}

	// Deleting the parent ticket / workspace can no longer cascade-erase the audit trail.
	if _, err := pool.Exec(ctx, `DELETE FROM execution_tickets WHERE id=$1`, ticketID); err == nil {
		t.Error("deleting a ticket that has audit logs must be refused")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, wsID); err == nil {
		t.Error("deleting a workspace that has audit logs must be refused")
	}
}

func TestConstraintsAndTriggers(t *testing.T) {
	pool := dbtest.AdminPool(t)
	ctx := context.Background()
	var wsID string
	if err := pool.QueryRow(ctx, `INSERT INTO workspaces(name) VALUES ('c') RETURNING id`).Scan(&wsID); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions(workspace_id, role_name, daily_budget_usd) VALUES ($1,'r',-1)`, wsID); err == nil {
		t.Error("negative budget must be rejected")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO execution_tickets(workspace_id, idempotency_key, action_type, payload, status) VALUES ($1,'a','x','{}','BOGUS')`, wsID); err == nil {
		t.Error("invalid ticket status must be rejected")
	}
	for i := 0; i < 2; i++ {
		_, err := pool.Exec(ctx, `INSERT INTO execution_tickets(workspace_id, idempotency_key, action_type, payload) VALUES ($1,'dup','x','{}')`, wsID)
		if (i == 0) != (err == nil) {
			t.Errorf("idempotency_key must be unique per workspace (attempt %d err=%v)", i, err)
		}
	}

	// updated_at trigger
	var before, after string
	_ = pool.QueryRow(ctx, `SELECT updated_at::text FROM workspaces WHERE id=$1`, wsID).Scan(&before)
	if _, err := pool.Exec(ctx, `SELECT pg_sleep(0.05)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspaces SET name='c2' WHERE id=$1`, wsID); err != nil {
		t.Fatal(err)
	}
	_ = pool.QueryRow(ctx, `SELECT updated_at::text FROM workspaces WHERE id=$1`, wsID).Scan(&after)
	if before == after {
		t.Error("updated_at must change on UPDATE")
	}
}

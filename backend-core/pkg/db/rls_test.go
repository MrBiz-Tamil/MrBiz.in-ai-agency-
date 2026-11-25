package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrbiz/abos/backend-core/pkg/db/dbtest"
	"github.com/mrbiz/abos/backend-core/pkg/tenant"
)

func count(t *testing.T, pool *pgxpool.Pool, ctx context.Context, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// The raw SQL below deliberately has NO workspace_id in any WHERE clause: isolation must come
// from row-level security alone.
func TestRowLevelSecurityIsolatesTenants(t *testing.T) {
	pool := dbtest.Pool(t) // abos_app role + tenant scoping, like production
	sys := tenant.System(context.Background())

	var a, b string
	if err := pool.QueryRow(sys, `INSERT INTO workspaces(name) VALUES ('a') RETURNING id`).Scan(&a); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(sys, `INSERT INTO workspaces(name) VALUES ('b') RETURNING id`).Scan(&b); err != nil {
		t.Fatal(err)
	}
	ctxA, ctxB := tenant.With(context.Background(), a), tenant.With(context.Background(), b)
	for _, w := range []struct {
		ctx context.Context
		id  string
	}{{ctxA, a}, {ctxB, b}} {
		for i := 0; i < 2; i++ {
			if _, err := pool.Exec(w.ctx, `INSERT INTO execution_tickets(workspace_id, idempotency_key, action_type, payload) VALUES ($1, 'k'||$2::int::text, 'x', '{}')`, w.id, int32(i)); err != nil {
				t.Fatalf("insert as own workspace: %v", err)
			}
		}
	}

	if n := count(t, pool, ctxA, "execution_tickets"); n != 2 {
		t.Errorf("tenant A must see exactly its 2 tickets, saw %d", n)
	}
	if n := count(t, pool, ctxB, "execution_tickets"); n != 2 {
		t.Errorf("tenant B must see exactly its 2 tickets, saw %d", n)
	}
	if n := count(t, pool, context.Background(), "execution_tickets"); n != 0 {
		t.Errorf("with no tenant set the tables must fail closed, saw %d rows", n)
	}
	if n := count(t, pool, sys, "execution_tickets"); n != 4 {
		t.Errorf("system context sees everything, saw %d", n)
	}
	if n := count(t, pool, ctxA, "workspaces"); n != 1 {
		t.Errorf("a tenant sees only its own workspace row, saw %d", n)
	}

	// Writes are fenced too: A cannot insert into, update or delete B's data.
	if _, err := pool.Exec(ctxA, `INSERT INTO execution_tickets(workspace_id, idempotency_key, action_type, payload) VALUES ($1,'evil','x','{}')`, b); err == nil {
		t.Error("tenant A must not be able to insert a row owned by B (WITH CHECK)")
	}
	if tag, _ := pool.Exec(ctxA, `UPDATE execution_tickets SET status='FAILED' WHERE workspace_id=$1`, b); tag.RowsAffected() != 0 {
		t.Errorf("tenant A must not update B's rows, affected %d", tag.RowsAffected())
	}
	if tag, _ := pool.Exec(ctxA, `DELETE FROM execution_tickets WHERE workspace_id=$1`, b); tag.RowsAffected() != 0 {
		t.Errorf("tenant A must not delete B's rows, affected %d", tag.RowsAffected())
	}
	if n := count(t, pool, ctxB, "execution_tickets"); n != 2 {
		t.Errorf("B's data must be untouched, saw %d", n)
	}

	// A connection reused for another tenant never leaks the previous tenant's scope
	// (pool max is 1 here, so these alternate on the same session).
	for i := 0; i < 3; i++ {
		if n := count(t, pool, ctxA, "execution_tickets"); n != 2 {
			t.Fatalf("iteration %d: A saw %d", i, n)
		}
		if n := count(t, pool, context.Background(), "execution_tickets"); n != 0 {
			t.Fatalf("iteration %d: unscoped call saw %d rows left over from A's scope", i, n)
		}
	}
}

func TestAppRoleCannotBypassRLSOrChangeSchema(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `ALTER TABLE execution_tickets DISABLE ROW LEVEL SECURITY`); err == nil {
		t.Error("the application role must not be able to disable RLS")
	}
	if _, err := pool.Exec(ctx, `SET app.bypass_rls = 'on'`); err != nil {
		t.Skipf("custom GUC not settable: %v", err)
	}
	// Even if a bug let a request set the GUC itself, the next acquire resets it to 'off'.
	if n := count(t, pool, context.Background(), "execution_tickets"); n != 0 {
		t.Errorf("bypass flag must be reset on acquire, saw %d rows", n)
	}
}

package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrbiz/venture-os/internal/tenancy"
	"github.com/mrbiz/venture-os/internal/testdb"
)

var asApp = tenancy.Options{AssumeRole: testdb.Role}

// run executes fn as venture_app inside a transaction carrying scope. RLS applies exactly as in production.
func run(t *testing.T, pool *pgxpool.Pool, scope tenancy.Scope, fn func(tx pgx.Tx) error) error {
	t.Helper()
	return tenancy.WithScopeTx(context.Background(), pool, scope, asApp, fn)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func wantErr(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q, got none", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("expected error containing %q, got: %v", substr, err)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, scope tenancy.Scope, table string) int {
	t.Helper()
	var n int
	must(t, run(t, pool, scope, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n)
	}))
	return n
}

// newTenant creates an organization exactly the way the control plane does: generate the uuid, scope to it,
// insert the org and its founder membership in one transaction.
func newTenant(t *testing.T, pool *pgxpool.Pool, slug, founder string) tenancy.Scope {
	t.Helper()
	id := uuid.New()
	scope := tenancy.Scope{TenantID: id, UserSub: founder}
	must(t, run(t, pool, scope, func(tx pgx.Tx) error {
		ctx := context.Background()
		if _, err := tx.Exec(ctx,
			`INSERT INTO organizations (id, slug, name, created_by, idempotency_key) VALUES ($1,$2,$3,$4,$5)`,
			id, slug, "Co "+slug, founder, "create-"+slug); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO tenant_members (tenant_id, user_sub, role) VALUES ($1,$2,'founder')`, id, founder)
		return err
	}))
	return scope
}

func TestSchemaIsolatesTenants(t *testing.T) {
	pool := testdb.Pool(t)
	a := newTenant(t, pool, "acme-health", "user-a")
	b := newTenant(t, pool, "globex-exim", "user-b")

	for _, s := range []tenancy.Scope{a, b} {
		for i, key := range []string{"k1", "k2"} {
			must(t, run(t, pool, s, func(tx pgx.Tx) error {
				_, err := tx.Exec(context.Background(),
					`INSERT INTO cashflow_items (tenant_id, direction, category, amount_minor, currency, due_date, idempotency_key)
					 VALUES ($1,'INFLOW','sales',$2,'USD',current_date,$3)`, s.TenantID, 1000*(i+1), key)
				return err
			}))
		}
	}

	t.Run("each tenant sees only its own rows without any WHERE clause", func(t *testing.T) {
		if n := count(t, pool, a, "cashflow_items"); n != 2 {
			t.Errorf("tenant A sees %d rows, want 2", n)
		}
		if n := count(t, pool, a, "organizations"); n != 1 {
			t.Errorf("tenant A sees %d organizations, want 1", n)
		}
	})

	t.Run("no scope fails closed", func(t *testing.T) {
		tx, err := pool.Begin(context.Background())
		must(t, err)
		defer tx.Rollback(context.Background())
		_, err = tx.Exec(context.Background(), "SET LOCAL ROLE "+testdb.Role)
		must(t, err)
		var n int
		must(t, tx.QueryRow(context.Background(), "SELECT count(*) FROM cashflow_items").Scan(&n))
		if n != 0 {
			t.Errorf("unscoped session saw %d rows, want 0", n)
		}
	})

	t.Run("portfolio read spans tenants but cannot write", func(t *testing.T) {
		portfolio := tenancy.Scope{ReadTenantIDs: []uuid.UUID{a.TenantID, b.TenantID}}
		if n := count(t, pool, portfolio, "cashflow_items"); n != 4 {
			t.Errorf("portfolio sees %d rows, want 4", n)
		}
		err := run(t, pool, portfolio, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(),
				`INSERT INTO cashflow_items (tenant_id, direction, category, amount_minor, currency, due_date, idempotency_key)
				 VALUES ($1,'INFLOW','x',1,'USD',current_date,'p1')`, a.TenantID)
			return err
		})
		wantErr(t, err, "row-level security")
	})

	t.Run("write scope plus read widening cannot touch another tenant's rows", func(t *testing.T) {
		both := tenancy.Scope{TenantID: a.TenantID, ReadTenantIDs: []uuid.UUID{a.TenantID, b.TenantID}}
		var updated, deleted int64
		must(t, run(t, pool, both, func(tx pgx.Tx) error {
			ct, err := tx.Exec(context.Background(), `UPDATE cashflow_items SET memo='hacked' WHERE tenant_id = $1`, b.TenantID)
			if err != nil {
				return err
			}
			updated = ct.RowsAffected()
			ct, err = tx.Exec(context.Background(), `DELETE FROM tenant_integrations WHERE tenant_id = $1`, b.TenantID)
			deleted = ct.RowsAffected()
			return err
		}))
		if updated != 0 || deleted != 0 {
			t.Errorf("cross-tenant write affected rows: update=%d delete=%d", updated, deleted)
		}
	})

	t.Run("cannot insert a row for another tenant", func(t *testing.T) {
		err := run(t, pool, a, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(),
				`INSERT INTO cashflow_items (tenant_id, direction, category, amount_minor, currency, due_date, idempotency_key)
				 VALUES ($1,'OUTFLOW','x',1,'USD',current_date,'evil')`, b.TenantID)
			return err
		})
		wantErr(t, err, "row-level security")
	})

	t.Run("tenant_id is immutable", func(t *testing.T) {
		err := run(t, pool, a, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `UPDATE cashflow_items SET tenant_id = $1`, b.TenantID)
			return err
		})
		if err == nil {
			t.Fatal("moving a row to another tenant must fail")
		}
	})

	t.Run("a user discovers their own memberships by sub", func(t *testing.T) {
		var ids []uuid.UUID
		must(t, run(t, pool, tenancy.Scope{UserSub: "user-a"}, func(tx pgx.Tx) error {
			rows, err := tx.Query(context.Background(), `SELECT tenant_id FROM tenant_members`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id uuid.UUID
				if err := rows.Scan(&id); err != nil {
					return err
				}
				ids = append(ids, id)
			}
			return rows.Err()
		}))
		if len(ids) != 1 || ids[0] != a.TenantID {
			t.Errorf("user-a memberships = %v, want [%s]", ids, a.TenantID)
		}
	})

	t.Run("composite foreign keys refuse cross-tenant references", func(t *testing.T) {
		var bpB uuid.UUID
		must(t, run(t, pool, b, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(),
				`INSERT INTO company_blueprints (tenant_id, version, schema_version, blueprint, created_by, idempotency_key)
				 VALUES ($1,1,'1','{}','user-b','bp1') RETURNING id`, b.TenantID).Scan(&bpB)
		}))
		err := run(t, pool, a, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(),
				`INSERT INTO crm_pipelines (tenant_id, key, name, entity_type, stages, blueprint_id, idempotency_key)
				 VALUES ($1,'sales','Sales','DEAL','["new"]',$2,'pl1')`, a.TenantID, bpB)
			return err
		})
		if sqlState(err) != "23503" {
			t.Fatalf("want foreign_key_violation (23503), got %v", err)
		}
	})

	t.Run("idempotency keys are per tenant and unique", func(t *testing.T) {
		// 'k1' already exists for both tenants (one each) - fine. Repeating it within a tenant must conflict.
		err := run(t, pool, a, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(),
				`INSERT INTO cashflow_items (tenant_id, direction, category, amount_minor, currency, due_date, idempotency_key)
				 VALUES ($1,'INFLOW','sales',5,'USD',current_date,'k1')`, a.TenantID)
			return err
		})
		if sqlState(err) != "23505" {
			t.Fatalf("want unique_violation (23505), got %v", err)
		}
	})
}

func TestLedgerIsBalancedAndAppendOnly(t *testing.T) {
	pool := testdb.Pool(t)
	a := newTenant(t, pool, "ledger-co", "u")

	var cash, revenue uuid.UUID
	must(t, run(t, pool, a, func(tx pgx.Tx) error {
		ctx := context.Background()
		if err := tx.QueryRow(ctx, `INSERT INTO ledger_accounts (tenant_id, code, name, type) VALUES ($1,'1000','Cash','ASSET') RETURNING id`, a.TenantID).Scan(&cash); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO ledger_accounts (tenant_id, code, name, type) VALUES ($1,'4000','Revenue','REVENUE') RETURNING id`, a.TenantID).Scan(&revenue)
	}))

	post := func(key string, n int, debit, credit int64) error {
		return run(t, pool, a, func(tx pgx.Tx) error {
			ctx := context.Background()
			var j uuid.UUID
			if err := tx.QueryRow(ctx,
				`INSERT INTO ledger_journals (tenant_id, entry_count, created_by, idempotency_key) VALUES ($1,$2,'u',$3) RETURNING id`,
				a.TenantID, n, key).Scan(&j); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO ledger_entries (tenant_id, journal_id, line_no, account_id, debit_minor, currency) VALUES ($1,$2,1,$3,$4,'USD')`,
				a.TenantID, j, cash, debit); err != nil {
				return err
			}
			_, err := tx.Exec(ctx,
				`INSERT INTO ledger_entries (tenant_id, journal_id, line_no, account_id, credit_minor, currency) VALUES ($1,$2,2,$3,$4,'USD')`,
				a.TenantID, j, revenue, credit)
			return err
		})
	}

	t.Run("balanced journal posts", func(t *testing.T) { must(t, post("j-ok", 2, 5000, 5000)) })
	t.Run("unbalanced journal is rejected at commit", func(t *testing.T) {
		wantErr(t, post("j-bad", 2, 5000, 4999), "not balanced")
	})
	t.Run("journal with fewer lines than declared is rejected", func(t *testing.T) {
		wantErr(t, post("j-short", 3, 100, 100), "declares 3 entries")
	})
	t.Run("a one-sided line must be strictly positive", func(t *testing.T) {
		if err := post("j-zero", 2, 0, 0); err == nil {
			t.Fatal("zero-amount entry accepted")
		}
	})
	t.Run("rejected journals leave nothing behind", func(t *testing.T) {
		if n := count(t, pool, a, "ledger_journals"); n != 1 {
			t.Errorf("journals = %d, want 1", n)
		}
	})
	t.Run("posted entries cannot be changed or removed", func(t *testing.T) {
		err := run(t, pool, a, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `UPDATE ledger_entries SET debit_minor = 1 WHERE line_no = 1`)
			return err
		})
		if err == nil {
			t.Error("UPDATE on ledger_entries succeeded")
		}
		err = run(t, pool, a, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `DELETE FROM ledger_journals`)
			return err
		})
		if err == nil {
			t.Error("DELETE on ledger_journals succeeded")
		}
	})
	t.Run("a balanced pair cannot be appended to a posted journal", func(t *testing.T) {
		err := run(t, pool, a, func(tx pgx.Tx) error {
			ctx := context.Background()
			var j uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM ledger_journals LIMIT 1`).Scan(&j); err != nil {
				return err
			}
			_, err := tx.Exec(ctx,
				`INSERT INTO ledger_entries (tenant_id, journal_id, line_no, account_id, debit_minor, currency) VALUES ($1,$2,3,$3,1,'USD')`,
				a.TenantID, j, cash)
			return err
		})
		wantErr(t, err, "declares 2 entries")
	})
}

func TestAuditLogIsAppendOnly(t *testing.T) {
	pool := testdb.Pool(t)
	a := newTenant(t, pool, "audit-co", "u")
	must(t, run(t, pool, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(),
			`INSERT INTO audit_logs (tenant_id, actor_type, actor_id, action) VALUES ($1,'USER','u','tenant.create')`, a.TenantID)
		return err
	}))
	for _, stmt := range []string{`UPDATE audit_logs SET action = 'x'`, `DELETE FROM audit_logs`, `TRUNCATE audit_logs`} {
		err := run(t, pool, a, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), stmt)
			return err
		})
		if err == nil {
			t.Errorf("%q succeeded for venture_app", stmt)
		}
	}
	if n := count(t, pool, a, "audit_logs"); n != 1 {
		t.Errorf("audit rows = %d, want 1", n)
	}
}

func TestApprovalStateMachine(t *testing.T) {
	pool := testdb.Pool(t)
	a := newTenant(t, pool, "approvals-co", "founder")

	var id uuid.UUID
	must(t, run(t, pool, a, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`INSERT INTO approval_queue (tenant_id, kind, title, requested_by_type, requested_by, payload, amount_minor, currency, idempotency_key)
			 VALUES ($1,'QUOTE','Quote #1','AGENT','sales-agent','{"sku":"A","qty":3}',150000,'USD','q1') RETURNING id`,
			a.TenantID).Scan(&id)
	}))

	update := func(sql string, args ...any) (int64, error) {
		var n int64
		err := run(t, pool, a, func(tx pgx.Tx) error {
			ct, err := tx.Exec(context.Background(), sql, args...)
			n = ct.RowsAffected()
			return err
		})
		return n, err
	}

	t.Run("payload hash is computed by the database", func(t *testing.T) {
		var h []byte
		must(t, run(t, pool, a, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT payload_hash FROM approval_queue WHERE id=$1`, id).Scan(&h)
		}))
		if len(h) != 32 {
			t.Fatalf("payload_hash length = %d, want 32", len(h))
		}
	})
	t.Run("new approvals must start pending", func(t *testing.T) {
		err := run(t, pool, a, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(),
				`INSERT INTO approval_queue (tenant_id, kind, title, requested_by_type, requested_by, payload, status, decided_by, decided_at, idempotency_key)
				 VALUES ($1,'QUOTE','x','AGENT','a','{}','APPROVED','me',now(),'sneaky')`, a.TenantID)
			return err
		})
		if err == nil {
			t.Fatal("inserted an already-approved approval")
		}
	})
	t.Run("the requested payload cannot be edited", func(t *testing.T) {
		_, err := update(`UPDATE approval_queue SET payload = '{"sku":"A","qty":300}' WHERE id=$1`, id)
		wantErr(t, err, "immutable")
	})
	t.Run("cannot skip approval", func(t *testing.T) {
		_, err := update(`UPDATE approval_queue SET status='EXECUTING', decided_by='x', decided_at=now() WHERE id=$1`, id)
		wantErr(t, err, "illegal approval transition")
	})
	t.Run("approving requires a decider", func(t *testing.T) {
		_, err := update(`UPDATE approval_queue SET status='APPROVED' WHERE id=$1`, id)
		if err == nil {
			t.Fatal("approved without decided_by")
		}
	})
	t.Run("approve then execute exactly once (compare-and-set)", func(t *testing.T) {
		n, err := update(`UPDATE approval_queue SET status='APPROVED', decided_by='founder', decided_at=now() WHERE id=$1 AND status='PENDING_APPROVAL'`, id)
		must(t, err)
		if n != 1 {
			t.Fatalf("approve affected %d rows", n)
		}
		// Replaying the same decision is a no-op, not a second approval.
		n, err = update(`UPDATE approval_queue SET status='APPROVED', decided_by='founder', decided_at=now() WHERE id=$1 AND status='PENDING_APPROVAL'`, id)
		must(t, err)
		if n != 0 {
			t.Fatalf("second approve affected %d rows, want 0", n)
		}
		n, err = update(`UPDATE approval_queue SET status='EXECUTING' WHERE id=$1 AND status='APPROVED'`, id)
		must(t, err)
		if n != 1 {
			t.Fatalf("first executor claimed %d rows, want 1", n)
		}
		n, err = update(`UPDATE approval_queue SET status='EXECUTING' WHERE id=$1 AND status='APPROVED'`, id)
		must(t, err)
		if n != 0 {
			t.Fatalf("second executor claimed %d rows, want 0", n)
		}
		_, err = update(`UPDATE approval_queue SET status='EXECUTED', executed_at=now(), execution_result='{"invoice":"INV-1"}' WHERE id=$1`, id)
		must(t, err)
	})
	t.Run("executed approvals are final and the decision is immutable", func(t *testing.T) {
		_, err := update(`UPDATE approval_queue SET status='EXECUTING' WHERE id=$1`, id)
		wantErr(t, err, "illegal approval transition")
		_, err = update(`UPDATE approval_queue SET decided_by='someone-else' WHERE id=$1`, id)
		wantErr(t, err, "decision is immutable")
	})
}

func TestBlueprintFreezesOnApproval(t *testing.T) {
	pool := testdb.Pool(t)
	a := newTenant(t, pool, "blueprint-co", "founder")

	var id uuid.UUID
	must(t, run(t, pool, a, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`INSERT INTO company_blueprints (tenant_id, version, schema_version, blueprint, created_by, idempotency_key)
			 VALUES ($1,1,'1','{"industry":"healthcare"}','founder','b1') RETURNING id`, a.TenantID).Scan(&id)
	}))
	exec := func(sql string, args ...any) error {
		return run(t, pool, a, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), sql, args...)
			return err
		})
	}

	must(t, exec(`UPDATE company_blueprints SET blueprint='{"industry":"retail"}' WHERE id=$1`, id)) // draft: editable
	must(t, exec(`UPDATE company_blueprints SET status='APPROVED', approved_by='founder', approved_at=now() WHERE id=$1`, id))
	wantErr(t, exec(`UPDATE company_blueprints SET blueprint='{"industry":"crypto"}' WHERE id=$1`, id), "frozen")
	wantErr(t, exec(`UPDATE company_blueprints SET status='DRAFT' WHERE id=$1`, id), "illegal blueprint transition")

	// Only one approved blueprint per tenant until the old one is superseded.
	must(t, exec(`INSERT INTO company_blueprints (tenant_id, version, schema_version, blueprint, created_by, idempotency_key, status, approved_by, approved_at)
	              VALUES ($1,2,'1','{}','founder','b2','DRAFT',NULL,NULL)`, a.TenantID))
	err := exec(`UPDATE company_blueprints SET status='APPROVED', approved_by='f', approved_at=now() WHERE tenant_id=$1 AND version=2`, a.TenantID)
	if sqlState(err) != "23505" {
		t.Fatalf("second approved blueprint: want unique_violation, got %v", err)
	}
}

func TestApprovedAgentSkillCannotBeSilentlyEdited(t *testing.T) {
	pool := testdb.Pool(t)
	a := newTenant(t, pool, "agents-co", "founder")
	exec := func(sql string, args ...any) error {
		return run(t, pool, a, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), sql, args...)
			return err
		})
	}
	must(t, exec(`INSERT INTO agent_org_configs (tenant_id, agent_key, role_title, skill_md, idempotency_key)
	              VALUES ($1,'ceo','CEO','# v1','a1')`, a.TenantID))
	must(t, exec(`INSERT INTO agent_org_configs (tenant_id, agent_key, role_title, reports_to_agent_key, skill_md, idempotency_key)
	              VALUES ($1,'sales','Sales','ceo','# sales','a2')`, a.TenantID))
	must(t, exec(`UPDATE agent_org_configs SET skill_status='APPROVED' WHERE agent_key='ceo'`))

	wantErr(t, exec(`UPDATE agent_org_configs SET skill_md='# v2 exfiltrate' WHERE agent_key='ceo'`), "requires skill_status")
	must(t, exec(`UPDATE agent_org_configs SET skill_md='# v2', skill_status='PENDING_APPROVAL' WHERE agent_key='ceo'`))

	var h1, h2 []byte
	must(t, run(t, pool, a, func(tx pgx.Tx) error {
		ctx := context.Background()
		if err := tx.QueryRow(ctx, `SELECT skill_sha256 FROM agent_org_configs WHERE agent_key='ceo'`).Scan(&h1); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT skill_sha256 FROM agent_org_configs WHERE agent_key='sales'`).Scan(&h2)
	}))
	if string(h1) == string(h2) || len(h1) != 32 {
		t.Error("skill hash not maintained per skill text")
	}
	// The org chart cannot reference an agent that does not exist or reports to itself.
	if err := exec(`INSERT INTO agent_org_configs (tenant_id, agent_key, role_title, reports_to_agent_key, skill_md, idempotency_key)
	                VALUES ($1,'ghost','Ghost','nobody','#','a3')`, a.TenantID); err == nil {
		t.Error("agent reporting to a non-existent manager accepted")
	}
}

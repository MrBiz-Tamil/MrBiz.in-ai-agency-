package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrbiz/venture-os/internal/crm"
	"github.com/mrbiz/venture-os/internal/crm/builtin"
	"github.com/mrbiz/venture-os/internal/crm/crmtest"
	"github.com/mrbiz/venture-os/internal/tenancy"
	"github.com/mrbiz/venture-os/internal/testdb"
)

var asApp = tenancy.Options{AssumeRole: testdb.Role}

type backend struct {
	*builtin.Provider
	pool *pgxpool.Pool
}

// NewTenant does what provisioning does: creates the organization and writes its blueprint's pipelines.
func (b backend) NewTenant(t *testing.T, ps []crm.Pipeline) uuid.UUID {
	t.Helper()
	id := uuid.New()
	scope := tenancy.Scope{TenantID: id, UserSub: "tester"}
	err := tenancy.WithScopeTx(context.Background(), b.pool, scope, asApp, func(tx pgx.Tx) error {
		ctx := context.Background()
		slug := "t" + strings.ReplaceAll(id.String(), "-", "")[:20]
		if _, err := tx.Exec(ctx, `INSERT INTO organizations (id, slug, name, created_by, idempotency_key) VALUES ($1,$2,$3,'tester',$4)`,
			id, slug, "Tenant "+slug, "create-"+slug); err != nil {
			return err
		}
		for i, p := range ps {
			stages, _ := json.Marshal(p.Stages)
			fields := []byte("[]")
			if len(p.CustomFields) > 0 {
				fields, _ = json.Marshal(p.CustomFields)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO crm_pipelines (tenant_id, key, name, entity_type, stages, custom_fields, status, idempotency_key, position)
				 VALUES ($1,$2,$3,$4,$5,$6,'ACTIVE',$7,$8)`,
				id, p.Key, p.Name, string(p.EntityType), stages, fields, "pipeline-"+p.Key, i); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	return id
}

func newBackend(t *testing.T) backend {
	pool := testdb.Pool(t)
	warm(t, pool)
	return backend{Provider: builtin.New(pool, asApp), pool: pool}
}

// warm opens every connection the pool may use, at once. A cold pool opens connections lazily and slowly, which makes
// concurrent callers queue behind each other and hides races. It does nothing on the single-connection test database.
func warm(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	n := int(pool.Config().MaxConns)
	if n > 8 {
		n = 8
	}
	var conns []*pgxpool.Conn
	for i := 0; i < n; i++ {
		c, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Release()
	}
}

// The built-in CRM passes the same contract as the in-memory reference, against real PostgreSQL with RLS enforced.
func TestContract(t *testing.T) {
	crmtest.Run(t, func(t *testing.T) crmtest.Backend { return newBackend(t) })
}

func TestUnknownTenantHasNoCRM(t *testing.T) {
	b := newBackend(t)
	if _, err := b.For(context.Background(), uuid.New()); err == nil {
		t.Fatal("a tenant that does not exist must not get a CRM")
	}
	if _, err := b.For(context.Background(), uuid.Nil); err == nil {
		t.Fatal("the nil tenant must not get a CRM")
	}
}

// The tests below go around the Go API to prove the DATABASE enforces the rules, so a bug in this package could
// not break them.

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func asTenant(t *testing.T, b backend, tenant uuid.UUID, fn func(tx pgx.Tx) error) error {
	t.Helper()
	return tenancy.WithScopeTx(context.Background(), b.pool, tenancy.Scope{TenantID: tenant}, asApp, fn)
}

func TestDatabaseEnforcesIsolationAndHistory(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t)
	ta, tb := b.NewTenant(t, crmtest.Pipelines()), b.NewTenant(t, crmtest.Pipelines())
	ca, _ := b.For(ctx, ta)
	cb, _ := b.For(ctx, tb)
	coA, _, err := ca.CreateCompany(ctx, "k1", crm.CompanyInput{Name: "Alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cb.CreateCompany(ctx, "k1", crm.CompanyInput{Name: "Beta"}); err != nil {
		t.Fatal(err)
	}
	dealA, _, err := ca.CreateDeal(ctx, "d1", crm.DealInput{Name: "Deal", CompanyID: coA.ID, Pipeline: "sales", Stage: "lead", Fields: map[string]string{"incoterm": "FOB"}})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a bare SELECT with no WHERE sees only the scoped tenant", func(t *testing.T) {
		var n int
		if err := asTenant(t, b, tb, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM crm_companies`).Scan(&n)
		}); err != nil || n != 1 {
			t.Fatalf("tenant B sees %d companies (err %v), want exactly its own 1", n, err)
		}
	})

	t.Run("no scope sees nothing and cannot write", func(t *testing.T) {
		tx, err := b.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE venture_app`); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM crm_companies`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("unscoped read saw %d rows (err %v)", n, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO crm_companies (tenant_id, name, idempotency_key, request_hash) VALUES ($1,'x','k',''::bytea)`, ta); err == nil {
			t.Fatal("an unscoped insert must be rejected by row-level security")
		}
	})

	t.Run("a composite foreign key refuses a cross-tenant reference even in raw SQL", func(t *testing.T) {
		err := asTenant(t, b, tb, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO crm_contacts (tenant_id, company_id, first_name, idempotency_key, request_hash) VALUES ($1,$2,'x','kk',''::bytea)`,
				tb, uuid.MustParse(string(coA.ID)))
			return err
		})
		if sqlState(err) != "23503" {
			t.Fatalf("want foreign_key_violation (23503), got %v", err)
		}
	})

	t.Run("there is no hard delete", func(t *testing.T) {
		err := asTenant(t, b, ta, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM crm_companies WHERE id = $1`, uuid.MustParse(string(coA.ID)))
			return err
		})
		if sqlState(err) != "42501" {
			t.Fatalf("want insufficient_privilege (42501), got %v", err)
		}
	})

	t.Run("a deal can never move to another company or pipeline", func(t *testing.T) {
		coA2, _, err := ca.CreateCompany(ctx, "k2", crm.CompanyInput{Name: "Alpha 2"})
		if err != nil {
			t.Fatal(err)
		}
		for name, set := range map[string]string{"company": `company_id = '` + string(coA2.ID) + `'`, "pipeline": `pipeline_key = 'suppliers'`} {
			err := asTenant(t, b, ta, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE crm_deals SET `+set+` WHERE id = $1`, uuid.MustParse(string(dealA.ID)))
				return err
			})
			if sqlState(err) != "23000" {
				t.Errorf("changing the deal's %s: want integrity_constraint_violation (23000), got %v", name, err)
			}
		}
	})

	t.Run("stage history cannot be rewritten or deleted", func(t *testing.T) {
		if _, err := ca.MoveDeal(ctx, dealA.ID, "qualified", "rep", 0); err != nil {
			t.Fatal(err)
		}
		for _, stmt := range []string{`UPDATE crm_deal_stage_history SET to_stage = 'won'`, `DELETE FROM crm_deal_stage_history`} {
			err := asTenant(t, b, ta, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, stmt); return err })
			if err == nil {
				t.Errorf("%q must be rejected", stmt)
			}
		}
		h, err := ca.DealHistory(ctx, dealA.ID)
		if err != nil || len(h) != 2 {
			t.Fatalf("history = %+v (err %v)", h, err)
		}
	})

	t.Run("an archived company keeps its row and its idempotent replay", func(t *testing.T) {
		if err := ca.ArchiveCompany(ctx, coA.ID, 0); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := asTenant(t, b, ta, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM crm_companies WHERE archived_at IS NOT NULL`).Scan(&n)
		}); err != nil || n != 1 {
			t.Fatalf("archived rows = %d (err %v)", n, err)
		}
		again, created, err := ca.CreateCompany(ctx, "k1", crm.CompanyInput{Name: "Alpha"})
		if err != nil || created || again.ID != coA.ID {
			t.Fatalf("replaying a create for an archived record must return it unchanged: %+v created=%v err=%v", again, created, err)
		}
	})
}

// A writer that bases an update on a version must not be able to overwrite a change another transaction has made but
// not yet committed. This is deterministic (no timing luck) and needs two connections, so it is skipped on the
// single-connection test database; run it with VENTURE_TEST_DATABASE_URL pointing at a real PostgreSQL.
//
// Without the row lock the stale writer reads the OLD committed version, passes its version check, blocks on the
// UPDATE until the other transaction commits, and then overwrites it silently.
func TestUpdateCannotOverwriteAConcurrentUncommittedChange(t *testing.T) {
	ctx := context.Background()
	b := newBackend(t)
	if b.pool.Config().MaxConns < 2 {
		t.Skip("needs a PostgreSQL with more than one connection (set VENTURE_TEST_DATABASE_URL)")
	}
	tenant := b.NewTenant(t, crmtest.Pipelines())
	c, err := b.For(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	co, _, err := c.CreateCompany(ctx, "lock-1", crm.CompanyInput{Name: "original"})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse(string(co.ID))

	locked, release, other := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		other <- asTenant(t, b, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE crm_companies SET name = 'by-other-writer', version = version + 1 WHERE id = $1`, id)
			close(locked)
			<-release
			return err
		})
	}()
	<-locked

	stale := make(chan error, 1)
	go func() {
		name := "stale-overwrite"
		_, err := c.UpdateCompany(ctx, co.ID, crm.CompanyPatch{Name: &name}, co.Version)
		stale <- err
	}()
	select {
	case err := <-stale:
		t.Fatalf("the update finished while another writer held the row: %v", err)
	case <-time.After(500 * time.Millisecond): // it is waiting for the row, as it should
	}
	close(release)
	if err := <-other; err != nil {
		t.Fatal(err)
	}
	if err := <-stale; !errors.Is(err, crm.ErrConflict) {
		t.Fatalf("a stale writer must get ErrConflict after the other commit, got %v", err)
	}
	got, err := c.GetCompany(ctx, co.ID)
	if err != nil || got.Name != "by-other-writer" || got.Version != co.Version+1 {
		t.Fatalf("the other writer's change was lost: %+v (%v)", got, err)
	}
}

package db_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrbiz/venture-os/internal/tenancy"
	"github.com/mrbiz/venture-os/internal/testdb"
)

func audit(t *testing.T, pool *pgxpool.Pool, s tenancy.Scope, action string, data string) {
	t.Helper()
	must(t, run(t, pool, s, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(),
			`INSERT INTO audit_logs (tenant_id, actor_type, actor_id, action, entity_type, entity_id, data) VALUES ($1,'USER','u1',$2,'x','1',$3::jsonb)`,
			s.TenantID, action, data)
		return err
	}))
}

type verdict struct {
	bad     *int64
	checked int64
	head    []byte
}

func verify(t *testing.T, pool *pgxpool.Pool, s tenancy.Scope) verdict {
	t.Helper()
	var v verdict
	must(t, run(t, pool, s, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT first_bad_id, rows_checked, head FROM audit_verify($1)`, s.TenantID).Scan(&v.bad, &v.checked, &v.head)
	}))
	return v
}

func asOwner(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%.70q: %v", sql, err)
	}
}

func TestAuditLogIsAHashChainPerTenant(t *testing.T) {
	pool := testdb.Pool(t)
	a := newTenant(t, pool, "chain-a", "user-a")
	b := newTenant(t, pool, "chain-b", "user-b")
	for i := 0; i < 5; i++ {
		audit(t, pool, a, fmt.Sprintf("act.%d", i), fmt.Sprintf(`{"n":%d}`, i))
	}
	audit(t, pool, b, "other.tenant", `{}`)

	t.Run("every row links to the one before, the first to nothing", func(t *testing.T) {
		var links int
		must(t, run(t, pool, a, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `
				SELECT count(*) FROM audit_logs c LEFT JOIN audit_logs p ON p.tenant_id = c.tenant_id AND p.hash = c.prev_hash
				 WHERE c.tenant_id = $1 AND c.action LIKE 'act.%' AND c.prev_hash IS NOT NULL AND p.id IS NOT NULL`, a.TenantID).Scan(&links)
		}))
		// 5 audit rows of ours plus the rows other code wrote while creating the tenant all chain; ours link at least 4 times.
		if links < 4 {
			t.Fatalf("only %d linked rows", links)
		}
		v := verify(t, pool, a)
		if v.bad != nil || v.checked < 5 || len(v.head) != 32 {
			t.Fatalf("a fresh chain must verify: %+v", v)
		}
	})

	t.Run("tenants have separate chains", func(t *testing.T) {
		vb := verify(t, pool, b)
		if vb.bad != nil || vb.checked < 1 {
			t.Fatalf("%+v", vb)
		}
		var prev []byte
		must(t, run(t, pool, b, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT prev_hash FROM audit_logs WHERE tenant_id=$1 AND action='other.tenant'`, b.TenantID).Scan(&prev)
		}))
		va := verify(t, pool, a)
		if bytes.Equal(va.head, vb.head) {
			t.Fatal("two tenants share a chain head")
		}
	})

	t.Run("the application cannot rewrite or delete audit rows", func(t *testing.T) {
		for _, stmt := range []string{`UPDATE audit_logs SET action = 'x'`, `DELETE FROM audit_logs`} {
			err := run(t, pool, a, func(tx pgx.Tx) error { _, e := tx.Exec(context.Background(), stmt); return e })
			if err == nil {
				t.Errorf("%q must be refused", stmt)
			}
		}
	})

	t.Run("the hash fields are unambiguous", func(t *testing.T) {
		var differ bool
		must(t, run(t, pool, a, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT audit_field('ab') || audit_field('c') <> audit_field('a') || audit_field('bc')`).Scan(&differ)
		}))
		if !differ {
			t.Fatal(`("ab","c") and ("a","bc") produce the same bytes`)
		}
	})

	t.Run("editing a row after the fact is detected, and undoing the edit heals it", func(t *testing.T) {
		var id int64
		var orig string
		must(t, run(t, pool, a, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT id, data::text FROM audit_logs WHERE tenant_id=$1 AND action='act.2'`, a.TenantID).Scan(&id, &orig)
		}))
		asOwner(t, pool, `ALTER TABLE audit_logs DISABLE TRIGGER USER`) // only a privileged operator can do this
		asOwner(t, pool, `UPDATE audit_logs SET data = '{"n": 999}' WHERE id = $1`, id)
		asOwner(t, pool, `ALTER TABLE audit_logs ENABLE TRIGGER USER`)
		v := verify(t, pool, a)
		if v.bad == nil || *v.bad != id {
			t.Fatalf("tampering with row %d was not detected: %+v", id, v)
		}
		asOwner(t, pool, `ALTER TABLE audit_logs DISABLE TRIGGER USER`)
		asOwner(t, pool, `UPDATE audit_logs SET data = $2::jsonb WHERE id = $1`, id, orig)
		asOwner(t, pool, `ALTER TABLE audit_logs ENABLE TRIGGER USER`)
		if v := verify(t, pool, a); v.bad != nil {
			t.Fatalf("restoring the data must restore the chain: %+v", v)
		}
	})

	t.Run("deleting a row from the middle breaks the link after it", func(t *testing.T) {
		var gone, next int64
		must(t, run(t, pool, a, func(tx pgx.Tx) error {
			if err := tx.QueryRow(context.Background(), `SELECT id FROM audit_logs WHERE tenant_id=$1 AND action='act.3'`, a.TenantID).Scan(&gone); err != nil {
				return err
			}
			return tx.QueryRow(context.Background(), `SELECT id FROM audit_logs WHERE tenant_id=$1 AND action='act.4'`, a.TenantID).Scan(&next)
		}))
		asOwner(t, pool, `ALTER TABLE audit_logs DISABLE TRIGGER USER`)
		asOwner(t, pool, `DELETE FROM audit_logs WHERE id = $1`, gone)
		asOwner(t, pool, `ALTER TABLE audit_logs ENABLE TRIGGER USER`)
		if v := verify(t, pool, a); v.bad == nil || *v.bad != next {
			t.Fatalf("deleting row %d must be detected at row %d: %+v", gone, next, v)
		}
	})
}

func TestAuditChainCannotDetectTruncationOfTheNewestRowsWithoutAnAnchor(t *testing.T) {
	// Documented limit, pinned by a test so nobody believes otherwise: removing the newest rows leaves a valid shorter
	// chain. The head hash is the anchor that makes it detectable once it is kept somewhere the database owner cannot write.
	pool := testdb.Pool(t)
	s := newTenant(t, pool, "trunc", "user-t")
	for i := 0; i < 3; i++ {
		audit(t, pool, s, fmt.Sprintf("t.%d", i), `{}`)
	}
	before := verify(t, pool, s)
	asOwner(t, pool, `ALTER TABLE audit_logs DISABLE TRIGGER USER`)
	asOwner(t, pool, `DELETE FROM audit_logs WHERE tenant_id=$1 AND action='t.2'`, s.TenantID)
	asOwner(t, pool, `ALTER TABLE audit_logs ENABLE TRIGGER USER`)
	after := verify(t, pool, s)
	if after.bad != nil {
		t.Fatalf("a truncated chain looks intact by itself: %+v", after)
	}
	if bytes.Equal(before.head, after.head) || after.checked != before.checked-1 {
		t.Fatalf("the head hash and row count must reveal the truncation to someone who anchored them: %+v vs %+v", before, after)
	}
}

func TestSpendCapsAreValidatedAndIsolated(t *testing.T) {
	pool := testdb.Pool(t)
	a := newTenant(t, pool, "caps-a", "user-a")
	b := newTenant(t, pool, "caps-b", "user-b")
	put := func(s tenancy.Scope, cur string, tx, hr, day any) error {
		return run(t, pool, s, func(q pgx.Tx) error {
			_, err := q.Exec(context.Background(),
				`INSERT INTO spend_caps (tenant_id, currency, per_transaction_minor, per_hour_minor, per_day_minor, updated_by) VALUES ($1,$2,$3,$4,$5,'u')`, s.TenantID, cur, tx, hr, day)
			return err
		})
	}
	must(t, put(a, "INR", 100000, 500000, 2000000))
	must(t, put(a, "USD", nil, nil, 1000000)) // a cap on one dimension only
	for name, err := range map[string]error{
		"hour above day":     put(b, "INR", nil, 5, 4),
		"transaction > hour": put(b, "USD", 10, 5, nil),
		"negative":           put(b, "EUR", -1, nil, nil),
		"bad currency":       put(b, "rupees", 1, 1, 1),
		"duplicate":          put(a, "INR", 1, 2, 3),
	} {
		if err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if n := count(t, pool, b, "spend_caps"); n != 0 {
		t.Fatalf("tenant B sees %d of tenant A's caps", n)
	}
	err := run(t, pool, b, func(tx pgx.Tx) error {
		_, e := tx.Exec(context.Background(), `UPDATE spend_caps SET per_day_minor = 99999999999 WHERE tenant_id = $1`, a.TenantID)
		return e
	})
	if err != nil {
		t.Fatalf("an update that matches no visible row is not an error: %v", err)
	}
	var day int64
	must(t, run(t, pool, a, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT per_day_minor FROM spend_caps WHERE currency='INR'`).Scan(&day)
	}))
	if day != 2000000 {
		t.Fatalf("tenant B changed tenant A's cap: %d", day)
	}
}

func TestSecretsAndConnectionsAreSealedConstrainedAndIsolated(t *testing.T) {
	pool := testdb.Pool(t)
	a := newTenant(t, pool, "sec-a", "user-a")
	b := newTenant(t, pool, "sec-b", "user-b")
	nonce, ct := bytes.Repeat([]byte{1}, 12), bytes.Repeat([]byte{2}, 40)
	putSecret := func(s tenancy.Scope, name string, n, c []byte) error {
		return run(t, pool, s, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `INSERT INTO tenant_secrets (tenant_id, name, key_version, nonce, ciphertext) VALUES ($1,$2,1,$3,$4)`, s.TenantID, name, n, c)
			return err
		})
	}
	must(t, putSecret(a, "google", nonce, ct))
	for name, err := range map[string]error{
		"short nonce":      putSecret(a, "zoom", nonce[:4], ct),
		"tiny ciphertext":  putSecret(a, "notion", nonce, []byte("x")),
		"bad name":         putSecret(a, "Bad Name!", nonce, ct),
		"duplicate secret": putSecret(a, "google", nonce, ct),
	} {
		if err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	conn := func(s tenancy.Scope, provider, secret string) error {
		return run(t, pool, s, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `INSERT INTO tenant_connections (tenant_id, provider, config, secret_name, connected_by) VALUES ($1,$2,'{"calendar_id":"primary"}',$3,'u')`, s.TenantID, provider, secret)
			return err
		})
	}
	must(t, conn(a, "google", "google"))
	if err := conn(a, "zoom", "no-such-secret"); sqlState(err) != "23503" {
		t.Fatalf("a connection must point at an existing secret: %v", err)
	}
	if err := conn(a, "slack", "google"); sqlState(err) != "23514" {
		t.Fatalf("an unknown provider: %v", err)
	}
	if err := conn(b, "google", "google"); sqlState(err) != "23503" {
		t.Fatalf("tenant B must not be able to point at tenant A's secret: %v", err)
	}
	if count(t, pool, b, "tenant_secrets") != 0 || count(t, pool, b, "tenant_connections") != 0 {
		t.Fatal("tenant B can see tenant A's secrets or connections")
	}
	_ = uuid.Nil
}

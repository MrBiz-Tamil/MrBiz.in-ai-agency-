package devdb_test

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrbiz/venture-os/internal/devdb"
	"github.com/mrbiz/venture-os/internal/testdb"
)

var ctx = context.Background()

func schemaPath() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "db", "schema.sql")
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("%.60q: %v", sql, err)
	}
}

func versions(t *testing.T, pool *pgxpool.Pool) []int {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT version FROM schema_version ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func hasTable(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestEnsureBuildsAnEmptyDatabaseAndIsIdempotent(t *testing.T) {
	pool := testdb.Pool(t) // superuser pool; start from nothing
	exec(t, pool, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)

	applied, err := devdb.Ensure(ctx, pool, schemaPath())
	if err != nil || !applied {
		t.Fatalf("fresh database: applied=%v err=%v", applied, err)
	}
	if v := versions(t, pool); len(v) != 4 || v[0] != 1 || v[1] != 2 || v[2] != 3 || v[3] != 5 {
		t.Fatalf("versions = %v, want [1 2 3 5]", v)
	}
	for _, tbl := range []string{"organizations", "crm_pipelines", "crm_companies", "crm_deals", "crm_deal_stage_history"} {
		if !hasTable(t, pool, tbl) {
			t.Errorf("table %s missing", tbl)
		}
	}
	if applied, err := devdb.Ensure(ctx, pool, schemaPath()); err != nil || applied {
		t.Fatalf("second run must do nothing: applied=%v err=%v", applied, err)
	}
}

// A dev database created before the CRM schema existed is upgraded in place, keeping its data.
func TestEnsureUpgradesADatabaseThatPredatesTheCRMSchema(t *testing.T) {
	pool := testdb.Pool(t)
	exec(t, pool, `
		DROP TABLE crm_deal_stage_history, crm_activities, crm_deals, crm_contacts, crm_companies CASCADE;
		DROP FUNCTION crm_forbid_identity_change();
		ALTER TABLE crm_pipelines DROP COLUMN position;
		DELETE FROM schema_version WHERE version = 2;
		INSERT INTO organizations (slug, name, created_by, idempotency_key) VALUES ('keepme-co', 'Keep Me', 'u', 'k')`)

	applied, err := devdb.Ensure(ctx, pool, schemaPath())
	if err != nil || !applied {
		t.Fatalf("upgrade: applied=%v err=%v", applied, err)
	}
	if v := versions(t, pool); len(v) != 4 {
		t.Fatalf("versions = %v", v)
	}
	if !hasTable(t, pool, "crm_companies") {
		t.Fatal("CRM tables were not added")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM organizations WHERE slug = 'keepme-co'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("existing data was lost: %d (%v)", n, err)
	}
	if applied, err := devdb.Ensure(ctx, pool, schemaPath()); err != nil || applied {
		t.Fatalf("after the upgrade nothing is left to apply: applied=%v err=%v", applied, err)
	}
}

// A dev database created before the agent work queue existed is upgraded in place, keeping its data.
func TestEnsureUpgradesADatabaseThatPredatesTheTaskQueue(t *testing.T) {
	pool := testdb.Pool(t)
	exec(t, pool, `
		DROP TABLE agent_task_events, agent_schedules, agent_tasks CASCADE;
		DROP FUNCTION agent_task_claim(text, int, timestamptz, interval, int, int), agent_task_expired(timestamptz, int),
		              agent_schedule_due(timestamptz, int), agent_schedule_fire(uuid, timestamptz),
		              agent_tasks_guard(), agent_task_events_guard(), agent_tasks_record_state();
		DELETE FROM schema_version WHERE version = 5;
		INSERT INTO organizations (slug, name, created_by, idempotency_key) VALUES ('keepme-too', 'Keep Me Too', 'u', 'k2')`)

	applied, err := devdb.Ensure(ctx, pool, schemaPath())
	if err != nil || !applied {
		t.Fatalf("upgrade: applied=%v err=%v", applied, err)
	}
	if v := versions(t, pool); len(v) != 4 || v[3] != 5 {
		t.Fatalf("versions = %v, want [1 2 3 5]", v)
	}
	for _, tbl := range []string{"agent_tasks", "agent_task_events", "agent_schedules"} {
		if !hasTable(t, pool, tbl) {
			t.Errorf("table %s was not added", tbl)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM organizations WHERE slug = 'keepme-too'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("existing data was lost: %d (%v)", n, err)
	}
}

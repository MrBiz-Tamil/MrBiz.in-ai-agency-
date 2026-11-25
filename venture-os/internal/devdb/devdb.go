// Package devdb prepares a local development database (for example PGlite from tools/pgtest) with the roles and
// schema Venture OS expects. It mirrors infra/postgres/init/01-bootstrap.sh plus db/apply.sql and is for
// development only: production databases are bootstrapped by the compose `postgres` and `schema` services.
package devdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Role is the unprivileged runtime role the application assumes.
const Role = "venture_app"

// files are the schema files a dev database needs, in order, with the schema_version row each one records.
// schema_rag.sql (pgvector) is production-only: PGlite has no extensions.
var files = []struct {
	version int
	name    string
}{
	{1, "schema.sql"},
	{2, "schema_crm.sql"},
	{3, "schema_controls.sql"},
	{5, "schema_tasks.sql"}, // version 4 is schema_rag.sql (pgvector), which has no place in a PGlite dev database
}

// Ensure creates the roles and applies any of db/schema.sql and db/schema_crm.sql that the database has not
// recorded yet (so a dev database created before the CRM schema existed is upgraded in place). schemaPath is the
// path of schema.sql; the other files are looked up beside it. It is a no-op on an up-to-date database. The pool
// must connect as a superuser.
func Ensure(ctx context.Context, pool *pgxpool.Pool, schemaPath string) (applied bool, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	simple := pgx.QueryExecModeSimpleProtocol

	have := map[int]bool{}
	var hasTable bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.schema_version') IS NOT NULL`).Scan(&hasTable); err != nil {
		return false, err
	}
	if hasTable {
		rows, err := conn.Query(ctx, `SELECT version FROM schema_version`)
		if err != nil {
			return false, err
		}
		for rows.Next() {
			var v int
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return false, err
			}
			have[v] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return false, err
		}
	}

	dir := filepath.Dir(filepath.Clean(schemaPath))
	for _, f := range files {
		if have[f.version] {
			continue
		}
		path := filepath.Join(dir, f.name)
		if f.version == 1 {
			path = filepath.Clean(schemaPath)
		}
		sql, err := os.ReadFile(path)
		if err != nil {
			return false, fmt.Errorf("read %s: %w", f.name, err)
		}
		if f.version == 1 {
			for _, stmt := range []string{
				`DO $$ BEGIN
				   IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='venture_owner') THEN CREATE ROLE venture_owner NOLOGIN NOSUPERUSER NOBYPASSRLS; END IF;
				   IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='venture_app')   THEN CREATE ROLE venture_app   NOLOGIN NOSUPERUSER NOBYPASSRLS; END IF;
				 END $$`,
				`GRANT ALL ON SCHEMA public TO venture_owner`,
				`GRANT USAGE ON SCHEMA public TO venture_app`,
			} {
				if _, err := conn.Exec(ctx, stmt, simple); err != nil {
					return false, fmt.Errorf("bootstrap: %w", err)
				}
			}
		}
		if err := applyAsOwner(ctx, conn, string(sql)); err != nil {
			return false, fmt.Errorf("apply %s: %w", f.name, err)
		}
		applied = true
	}
	return applied, nil
}

func applyAsOwner(ctx context.Context, conn *pgxpool.Conn, sql string) (err error) {
	simple := pgx.QueryExecModeSimpleProtocol
	if _, err := conn.Exec(ctx, `SET ROLE venture_owner`, simple); err != nil {
		return err
	}
	defer func() {
		if _, rerr := conn.Exec(context.WithoutCancel(ctx), `RESET ROLE`, simple); err == nil {
			err = rerr
		}
	}()
	_, err = conn.Exec(ctx, sql, simple)
	return err
}

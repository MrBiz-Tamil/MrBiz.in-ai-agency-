// Package db holds the embedded SQL migrations and a small migration runner.
package db

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Pool is what Up/Down operate on (satisfied by *pgxpool.Pool).
type Pool interface {
	Beginner
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type migration struct {
	version string // e.g. "000001_init_schema"
	sql     string
}

func load(suffix string) ([]migration, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		b, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: strings.TrimSuffix(e.Name(), suffix), sql: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func ensureTable(ctx context.Context, p Pool) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func applied(ctx context.Context, p Pool) (map[string]bool, error) {
	rows, err := p.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		m[v] = true
	}
	return m, rows.Err()
}

// Up applies every pending migration, each in its own transaction. It is idempotent.
func Up(ctx context.Context, p Pool) ([]string, error) {
	if err := ensureTable(ctx, p); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}
	migs, err := load(".up.sql")
	if err != nil {
		return nil, err
	}
	done, err := applied(ctx, p)
	if err != nil {
		return nil, err
	}
	var ran []string
	for _, m := range migs {
		if done[m.version] {
			continue
		}
		tx, err := p.Begin(ctx)
		if err != nil {
			return ran, err
		}
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			_ = tx.Rollback(ctx)
			return ran, fmt.Errorf("migration %s: %w", m.version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1)`, m.version); err != nil {
			_ = tx.Rollback(ctx)
			return ran, err
		}
		if err := tx.Commit(ctx); err != nil {
			return ran, err
		}
		ran = append(ran, m.version)
	}
	return ran, nil
}

// Down reverts the most recent `steps` applied migrations (newest first).
func Down(ctx context.Context, p Pool, steps int) ([]string, error) {
	if err := ensureTable(ctx, p); err != nil {
		return nil, err
	}
	ups, err := load(".down.sql")
	if err != nil {
		return nil, err
	}
	done, err := applied(ctx, p)
	if err != nil {
		return nil, err
	}
	var ran []string
	for i := len(ups) - 1; i >= 0 && steps > 0; i-- {
		m := ups[i]
		if !done[m.version] {
			continue
		}
		tx, err := p.Begin(ctx)
		if err != nil {
			return ran, err
		}
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			_ = tx.Rollback(ctx)
			return ran, fmt.Errorf("down %s: %w", m.version, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version=$1`, m.version); err != nil {
			_ = tx.Rollback(ctx)
			return ran, err
		}
		if err := tx.Commit(ctx); err != nil {
			return ran, err
		}
		ran = append(ran, m.version)
		steps--
	}
	return ran, nil
}

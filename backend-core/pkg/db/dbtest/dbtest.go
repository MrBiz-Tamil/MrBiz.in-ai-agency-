// Package dbtest gives integration tests a real, migrated PostgreSQL.
//
// It uses ABOS_TEST_DATABASE_URL if set (any Postgres 16; add pool_max_conns=1 for PGlite).
// Otherwise it starts the in-memory PGlite server from tools/pgtest when node and its
// dependencies are available, and skips the test when neither is.
package dbtest

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrbiz/abos/backend-core/pkg/db"
)

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// URL returns a database URL for tests, starting PGlite if needed. Skips if unavailable.
func URL(t *testing.T) string {
	t.Helper()
	if u := os.Getenv("ABOS_TEST_DATABASE_URL"); u != "" {
		return u
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no ABOS_TEST_DATABASE_URL and node not installed; skipping Postgres integration test")
	}
	dir := filepath.Join(repoRoot(), "tools", "pgtest")
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "@electric-sql")); err != nil {
		t.Skip("tools/pgtest dependencies not installed (cd tools/pgtest && npm install); skipping Postgres integration test")
	}
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "server.mjs", fmt.Sprint(port))
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatalf("start pgtest: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err == nil {
			_ = c.Close()
			return fmt.Sprintf("postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable&pool_max_conns=1&default_query_exec_mode=describe_exec", port)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("pgtest server did not start")
	return ""
}

func migrated(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	url := URL(t)
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close() // frees PGlite's single connection for the next pool
	if _, err := db.Up(ctx, admin); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	return url
}

// Pool returns a pool to a freshly migrated database, connected the way the application
// connects: as the non-superuser abos_app role with per-request tenant scoping, so
// row-level security is in force for everything the test does.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := migrated(t)
	pool, err := db.NewTenantPool(context.Background(), url, "abos_app")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// AdminPool returns a superuser pool to a freshly migrated database (RLS does not apply),
// for tests of the schema itself.
func AdminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := migrated(t)
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

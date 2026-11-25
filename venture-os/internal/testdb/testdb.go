// Package testdb gives integration tests a real PostgreSQL with db/schema.sql applied.
//
// It uses VENTURE_TEST_DATABASE_URL if set (a Postgres 15+ superuser URL; the roles and schema are created in
// it, so point it at a throwaway database). Otherwise it starts the in-process PGlite server in tools/pgtest
// when node and its dependencies exist, and skips the test when neither
// is available. PGlite has no extensions, which is why schema.sql and schema_crm.sql need none; schema_rag.sql
// (pgvector) is not applied here.
package testdb

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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Role is the runtime role tests assume, mirroring production's venture_app connection.
const Role = "venture_app"

func moduleRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func url(t *testing.T) string {
	t.Helper()
	if u := os.Getenv("VENTURE_TEST_DATABASE_URL"); u != "" {
		return u
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no VENTURE_TEST_DATABASE_URL and node not installed; skipping Postgres integration test")
	}
	dir := filepath.Join(moduleRoot(), "tools", "pgtest")
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

// Pool returns a superuser pool to a database with the roles from infra/postgres/init/01-bootstrap.sh and
// db/schema.sql applied as venture_owner. Use tenancy.Options{AssumeRole: testdb.Role} so RLS is enforced.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	simple := pgx.QueryExecModeSimpleProtocol
	mustExec := func(sql string) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, simple); err != nil {
			t.Fatalf("exec %.80q: %v", sql, err)
		}
	}
	// Same role model as the bootstrap script.
	mustExec(`DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	mustExec(`DROP ROLE IF EXISTS venture_app; DROP ROLE IF EXISTS venture_owner`)
	mustExec(`CREATE ROLE venture_owner NOLOGIN NOSUPERUSER NOBYPASSRLS; CREATE ROLE venture_app NOLOGIN NOSUPERUSER NOBYPASSRLS`)
	mustExec(`GRANT ALL ON SCHEMA public TO venture_owner; GRANT USAGE ON SCHEMA public TO venture_app`)

	mustExec(`SET ROLE venture_owner`)
	for _, name := range []string{"schema.sql", "schema_crm.sql", "schema_controls.sql", "schema_tasks.sql"} { // schema_rag.sql needs pgvector: production only
		schema, err := os.ReadFile(filepath.Join(moduleRoot(), "db", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := conn.Exec(ctx, string(schema), simple); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	mustExec(`RESET ROLE`)
	return pool
}

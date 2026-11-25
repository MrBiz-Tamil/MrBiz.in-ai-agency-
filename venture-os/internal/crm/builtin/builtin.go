// Package builtin is Venture OS's own CRM: the crm.CRM implementation backed by the crm_* tables in db/schema_crm.sql.
//
// Every call runs in a transaction scoped to one tenant (internal/tenancy), so PostgreSQL row-level security is the
// final authority on isolation; the explicit tenant_id predicates in the SQL are there for the planner and as a
// second line of defence, not as the mechanism. It passes the same contract suite (internal/crm/crmtest) as the
// in-memory reference and as every external adapter must.
package builtin

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mrbiz/venture-os/internal/crm"
	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/tenancy"
)

// Provider hands out tenant-bound CRM clients over one database.
type Provider struct {
	db   tenancy.Beginner
	opts tenancy.Options
}

var _ crm.Provider = (*Provider)(nil)

// New returns a provider. Production passes the application pool connected as venture_app and zero Options; tests
// connect as a superuser and set Options.AssumeRole so row-level security is genuinely enforced.
func New(db tenancy.Beginner, opts tenancy.Options) *Provider { return &Provider{db: db, opts: opts} }

// For returns the CRM of a tenant. It fails if the tenant does not exist.
func (p *Provider) For(ctx context.Context, tenant uuid.UUID) (crm.CRM, error) {
	if tenant == uuid.Nil {
		return nil, errors.New("builtin crm: nil tenant")
	}
	c := &client{db: p.db, opts: p.opts, tenant: tenant}
	err := c.tx(ctx, func(tx pgx.Tx) error {
		var one int
		return tx.QueryRow(ctx, `SELECT 1 FROM organizations WHERE id = $1`, tenant).Scan(&one)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("builtin crm: tenant %s does not exist", tenant)
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

type client struct {
	db     tenancy.Beginner
	opts   tenancy.Options
	tenant uuid.UUID
}

func (c *client) Capabilities() crm.Capabilities {
	return crm.Capabilities{OptimisticConcurrency: true, StageHistory: true}
}

// tx runs fn in a transaction carrying this client's tenant scope.
func (c *client) tx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return tenancy.WithScopeTx(ctx, c.db, tenancy.Scope{TenantID: c.tenant}, c.opts, fn)
}

func (c *client) Pipelines(ctx context.Context) ([]crm.Pipeline, error) {
	var out []crm.Pipeline
	err := c.tx(ctx, func(tx pgx.Tx) (err error) {
		out, err = loadPipelines(ctx, tx, c.tenant)
		return err
	})
	return out, err
}

func loadPipelines(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) ([]crm.Pipeline, error) {
	rows, err := tx.Query(ctx,
		`SELECT key, name, entity_type, stages, custom_fields FROM crm_pipelines
		  WHERE tenant_id = $1 AND status = 'ACTIVE' ORDER BY position, key`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []crm.Pipeline{}
	for rows.Next() {
		var p crm.Pipeline
		var et string
		if err := rows.Scan(&p.Key, &p.Name, &et, &p.Stages, &p.CustomFields); err != nil {
			return nil, err
		}
		p.EntityType = domain.EntityType(et)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ───────────────────────────── ids, errors, small helpers ─────────────────────────────

// parseID accepts only the canonical 36-character uuid form; anything else cannot be one of our ids.
func parseID(id crm.ID) (uuid.UUID, bool) {
	if len(id) != 36 {
		return uuid.Nil, false
	}
	u, err := uuid.Parse(string(id))
	return u, err == nil
}

func notFound(kind string, id crm.ID) error {
	return fmt.Errorf("%w: %s %q", crm.ErrNotFound, kind, id)
}

func invalidRef(format string, a ...any) error {
	return fmt.Errorf("%w: %s", crm.ErrInvalid, fmt.Sprintf(format, a...))
}

func checkVersion(ifVersion, current int64) error {
	if ifVersion != 0 && ifVersion != current {
		return fmt.Errorf("%w: record is at version %d, caller had %d", crm.ErrConflict, current, ifVersion)
	}
	return nil
}

// optUUID is the SQL argument for an optional reference: NULL when empty.
func optUUID(id crm.ID) (any, error) {
	if id == "" {
		return nil, nil
	}
	u, ok := parseID(id)
	if !ok {
		return nil, invalidRef("%q is not a valid id", id)
	}
	return u, nil
}

// requireRef checks that a parent record exists, is not archived, and (through row-level security) belongs to this
// tenant, and locks it against being archived before this transaction commits. Anything else is ErrInvalid: a bad
// reference in the input, as opposed to a missing record that was addressed directly.
func requireRef(ctx context.Context, tx pgx.Tx, table, what string, tenant uuid.UUID, id crm.ID) error {
	u, ok := parseID(id)
	if !ok {
		return invalidRef("%s %q does not exist", what, id)
	}
	var one int
	err := tx.QueryRow(ctx,
		`SELECT 1 FROM `+table+` WHERE tenant_id = $1 AND id = $2 AND archived_at IS NULL FOR SHARE`, tenant, u).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return invalidRef("%s %q does not exist", what, id)
	}
	return err
}

// fetch reads one row by id into dest. Missing, archived (unless includeArchived) and other tenants' rows are all
// ErrNotFound; lock takes a row lock for read-modify-write.
func fetch(ctx context.Context, tx pgx.Tx, table, cols string, archivable bool, tenant, id uuid.UUID, includeArchived, lock bool, dest ...any) error {
	q := `SELECT ` + cols + ` FROM ` + table + ` WHERE tenant_id = $1 AND id = $2`
	if archivable && !includeArchived {
		q += ` AND archived_at IS NULL`
	}
	if lock {
		q += ` FOR UPDATE`
	}
	err := tx.QueryRow(ctx, q, tenant, id).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return crm.ErrNotFound
	}
	return err
}

// replayID looks up an earlier create by idempotency key. It returns "" if the key is unused, the original record's
// id if the same request is being replayed, and ErrIdempotencyMismatch if the key was used for different input.
func replayID(ctx context.Context, tx pgx.Tx, table, kind string, tenant uuid.UUID, key string, hash [32]byte) (string, error) {
	var id string
	var stored []byte
	err := tx.QueryRow(ctx, `SELECT id::text, request_hash FROM `+table+` WHERE tenant_id = $1 AND idempotency_key = $2`, tenant, key).Scan(&id, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if string(stored) != string(hash[:]) {
		return "", fmt.Errorf("%w: key %q was used for a different %s", crm.ErrIdempotencyMismatch, key, kind)
	}
	return id, nil
}

// ───────────────────────────── listing ─────────────────────────────

// listQuery builds a keyset-paged SELECT. Every value goes through arg, so nothing from callers is ever spliced
// into SQL text; only the constant table, column and condition strings written in this package are.
type listQuery struct {
	table, cols string
	where       []string
	args        []any
}

func newList(table, cols string, tenant uuid.UUID, after int64, liveOnly bool) *listQuery {
	q := &listQuery{table: table, cols: cols}
	q.where = []string{"tenant_id = " + q.arg(tenant), "seq > " + q.arg(after)}
	if liveOnly {
		q.where = append(q.where, "archived_at IS NULL")
	}
	return q
}

func (q *listQuery) arg(v any) string {
	q.args = append(q.args, v)
	return "$" + strconv.Itoa(len(q.args))
}

func (q *listQuery) and(cond string) { q.where = append(q.where, cond) }

// idEq adds `col = id`. It reports false when id is malformed, meaning nothing can match.
func (q *listQuery) idEq(col string, id crm.ID) bool {
	if id == "" {
		return true
	}
	u, ok := parseID(id)
	if !ok {
		return false
	}
	q.and(col + " = " + q.arg(u))
	return true
}

func (q *listQuery) textEq(col, v string) {
	if v != "" {
		q.and(col + " = " + q.arg(v))
	}
}

// search adds a case-insensitive substring match over the given expressions. LIKE wildcards in the query are
// escaped, so "100%" finds "100%" and not everything that starts with "100".
func (q *listQuery) search(query string, exprs ...string) {
	query = strings.TrimSpace(query)
	if query == "" {
		return
	}
	ph := q.arg("%" + likeEscaper.Replace(query) + "%")
	parts := make([]string, len(exprs))
	for i, e := range exprs {
		parts[i] = e + " ILIKE " + ph
	}
	q.and("(" + strings.Join(parts, " OR ") + ")")
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func pageArgs(p crm.PageRequest) (size int, after int64, err error) {
	if size, err = crm.PageSize(p); err != nil {
		return 0, 0, err
	}
	after, err = crm.ParseSeqCursor(p.Cursor)
	return size, after, err
}

// runList executes q, oldest-created first, fetching one extra row to learn whether another page exists.
func runList[T any](ctx context.Context, tx pgx.Tx, q *listQuery, size int, newRow func() ([]any, func() T)) (crm.Page[T], error) {
	sql := `SELECT seq, ` + q.cols + ` FROM ` + q.table + ` WHERE ` + strings.Join(q.where, " AND ") +
		` ORDER BY seq LIMIT ` + strconv.Itoa(size+1)
	rows, err := tx.Query(ctx, sql, q.args...)
	if err != nil {
		return crm.Page[T]{}, err
	}
	defer rows.Close()
	var out crm.Page[T]
	var seqs []int64
	for rows.Next() {
		var seq int64
		dest, done := newRow()
		if err := rows.Scan(append([]any{&seq}, dest...)...); err != nil {
			return crm.Page[T]{}, err
		}
		out.Items = append(out.Items, done())
		seqs = append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		return crm.Page[T]{}, err
	}
	if len(out.Items) > size {
		out.Items = out.Items[:size]
		out.Next = crm.SeqCursor(seqs[size-1])
	}
	return out, nil
}

func set(dst *string, v *string) {
	if v != nil {
		*dst = *v
	}
}

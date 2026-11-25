// Package tenancy carries the caller's tenant scope through a request and applies it to Postgres.
//
// The scope is the ONLY thing row-level security looks at (see db/schema.sql). It is set with SET LOCAL
// semantics (set_config(..., is_local => true)), so it dies with the transaction and can never leak to the next
// request that reuses a pooled connection.
package tenancy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Scope is who is asking and which tenants they may touch.
type Scope struct {
	// TenantID is the single tenant the request acts in. Required for any write.
	TenantID uuid.UUID
	// ReadTenantIDs widens READS only (portfolio overview, global approval inbox). It must be derived from the
	// caller's verified memberships, never from request input. It never grants write access.
	ReadTenantIDs []uuid.UUID
	// UserSub is the authenticated OIDC subject; it lets a user list their own memberships.
	UserSub string
}

// ErrNoScope is returned when a database call is attempted without any scope.
var ErrNoScope = errors.New("tenancy: no tenant scope in context")

// Validate rejects scopes that could not be applied safely.
func (s Scope) Validate() error {
	if s.TenantID == uuid.Nil && len(s.ReadTenantIDs) == 0 && s.UserSub == "" {
		return ErrNoScope
	}
	for _, id := range s.ReadTenantIDs {
		if id == uuid.Nil {
			return errors.New("tenancy: nil uuid in ReadTenantIDs")
		}
	}
	if strings.ContainsAny(s.UserSub, "\x00") {
		return errors.New("tenancy: invalid user sub")
	}
	return nil
}

func (s Scope) readIDs() string {
	if len(s.ReadTenantIDs) == 0 {
		return ""
	}
	parts := make([]string, len(s.ReadTenantIDs))
	for i, id := range s.ReadTenantIDs {
		parts[i] = id.String()
	}
	return strings.Join(parts, ",")
}

type ctxKey struct{}

// WithScope returns ctx carrying scope.
func WithScope(ctx context.Context, s Scope) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// FromContext returns the scope in ctx, or ErrNoScope.
func FromContext(ctx context.Context) (Scope, error) {
	s, ok := ctx.Value(ctxKey{}).(Scope)
	if !ok {
		return Scope{}, ErrNoScope
	}
	return s, nil
}

// MustTenant returns the single acting tenant in ctx, or an error if the request has none.
func MustTenant(ctx context.Context) (uuid.UUID, error) {
	s, err := FromContext(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	if s.TenantID == uuid.Nil {
		return uuid.Nil, errors.New("tenancy: request has no acting tenant")
	}
	return s.TenantID, nil
}

// Beginner is satisfied by *pgxpool.Pool and pgx.Conn.
type Beginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Options tune WithTx.
type Options struct {
	// AssumeRole, if set, runs SET LOCAL ROLE <role> after BEGIN. Production connects directly as
	// venture_app and leaves this empty; tests connect as a superuser and drop to venture_app so that
	// row-level security is genuinely enforced.
	AssumeRole string
	TxOptions  pgx.TxOptions
}

// WithTx runs fn in a transaction whose Postgres session settings carry the scope in ctx. It commits if fn
// returns nil and rolls back otherwise.
func WithTx(ctx context.Context, db Beginner, opts Options, fn func(tx pgx.Tx) error) error {
	scope, err := FromContext(ctx)
	if err != nil {
		return err
	}
	return WithScopeTx(ctx, db, scope, opts, fn)
}

// WithScopeTx is WithTx with an explicit scope (used by bootstrap paths that have no request context).
func WithScopeTx(ctx context.Context, db Beginner, scope Scope, opts Options, fn func(tx pgx.Tx) error) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, opts.TxOptions)
	if err != nil {
		return fmt.Errorf("tenancy: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	if opts.AssumeRole != "" {
		if !validIdent(opts.AssumeRole) {
			return fmt.Errorf("tenancy: invalid role name %q", opts.AssumeRole)
		}
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+opts.AssumeRole); err != nil {
			return fmt.Errorf("tenancy: set role: %w", err)
		}
	}
	tenant := ""
	if scope.TenantID != uuid.Nil {
		tenant = scope.TenantID.String()
	}
	if _, err := tx.Exec(ctx,
		`SELECT set_config('app.tenant_id', $1, true),
		        set_config('app.tenant_ids', $2, true),
		        set_config('app.user_sub', $3, true)`,
		tenant, scope.readIDs(), scope.UserSub); err != nil {
		return fmt.Errorf("tenancy: apply scope: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

func validIdent(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for _, r := range s {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

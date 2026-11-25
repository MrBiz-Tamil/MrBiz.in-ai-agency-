package db

import (
	"context"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrbiz/abos/backend-core/pkg/tenant"
)

var roleRE = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// NewTenantPool opens a pool whose connections carry the caller's tenant for row-level
// security. Every time a connection is acquired, app.workspace_id / app.bypass_rls are set
// from the request context (see package tenant), so a query that forgot a WHERE clause still
// cannot read another workspace's rows.
//
// If role is non-empty each connection does SET ROLE <role> (use "abos_app": a non-owner,
// non-superuser role, which is what makes RLS apply).
func NewTenantPool(ctx context.Context, url, role string) (*pgxpool.Pool, error) {
	if role != "" && !roleRE.MatchString(role) {
		return nil, fmt.Errorf("invalid database role %q", role)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	if role != "" {
		cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
			_, err := c.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
			return err
		}
	}
	cfg.BeforeAcquire = func(ctx context.Context, c *pgx.Conn) bool {
		ws, system := tenant.From(ctx)
		bypass := "off"
		if system {
			bypass = "on"
		}
		_, err := c.Exec(ctx, "SELECT set_config('app.workspace_id', $1, false), set_config('app.bypass_rls', $2, false)", ws, bypass)
		return err == nil // a connection we could not scope is discarded, never used unscoped
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

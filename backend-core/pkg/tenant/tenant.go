// Package tenant carries the workspace a database call is scoped to. The Postgres pool reads
// it when a connection is acquired and sets the session variables that row-level security
// policies check, so tenant isolation is enforced by the database, not only by WHERE clauses.
package tenant

import "context"

type key int

const (
	workspaceKey key = iota
	systemKey
)

// With scopes ctx to one workspace.
func With(ctx context.Context, workspaceID string) context.Context {
	return context.WithValue(ctx, workspaceKey, workspaceID)
}

// System marks ctx as a trusted cross-tenant operation (migrations, startup policy loading,
// workspace creation). Never derive it from request data.
func System(ctx context.Context) context.Context {
	return context.WithValue(ctx, systemKey, true)
}

// From returns the scoped workspace ("" if none) and whether the context is a system context.
func From(ctx context.Context) (workspaceID string, system bool) {
	workspaceID, _ = ctx.Value(workspaceKey).(string)
	system, _ = ctx.Value(systemKey).(bool)
	return
}

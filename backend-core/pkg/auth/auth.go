// Package auth implements users, sessions, roles and request authentication.
package auth

import (
	"context"
	"errors"
)

type Role string

const (
	RoleOwner    Role = "owner"
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator"
	RoleAuditor  Role = "auditor"
)

type Permission string

const (
	PermRead    Permission = "read"    // view boards, tickets, audit trails, agents, goals
	PermDecide  Permission = "decide"  // approve / reject / modify held tickets
	PermOperate Permission = "operate" // pause/resume agents, heartbeats, goals, corrections
	PermAdmin   Permission = "admin"   // budgets, policies, MCP connections
)

var rolePerms = map[Role]map[Permission]bool{
	RoleOwner:    {PermRead: true, PermDecide: true, PermOperate: true, PermAdmin: true},
	RoleAdmin:    {PermRead: true, PermDecide: true, PermOperate: true, PermAdmin: true},
	RoleOperator: {PermRead: true, PermDecide: true, PermOperate: true},
	RoleAuditor:  {PermRead: true},
}

// Can reports whether the role holds the permission. Unknown roles hold nothing.
func (r Role) Can(p Permission) bool { return rolePerms[r][p] }

// Principal is the authenticated caller attached to each request.
type Principal struct {
	UserID      string `json:"userId"`
	WorkspaceID string `json:"workspaceId"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Role        Role   `json:"role"`
}

var (
	ErrUnauthenticated = errors.New("authentication required")
	ErrForbidden       = errors.New("forbidden")
	ErrRateLimited     = errors.New("too many attempts; try again later")
	ErrBadCredentials  = errors.New("invalid email or password")
)

type ctxKey struct{}

func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the authenticated principal, if any.
func FromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(*Principal)
	return p, ok && p != nil
}

// Require returns the principal if it holds the permission, otherwise a typed error.
func Require(ctx context.Context, perm Permission) (*Principal, error) {
	p, ok := FromContext(ctx)
	if !ok {
		return nil, ErrUnauthenticated
	}
	if !p.Role.Can(perm) {
		return nil, ErrForbidden
	}
	return p, nil
}

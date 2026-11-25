package graphql

import (
	"context"
	"errors"
	"log"

	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/mrbiz/abos/backend-core/pkg/auth"
	"github.com/mrbiz/abos/backend-core/pkg/db/repository"
	"github.com/mrbiz/abos/backend-core/pkg/events"
	agentv1 "github.com/mrbiz/abos/backend-core/pkg/gen/abos/agent/v1"
	"github.com/mrbiz/abos/backend-core/pkg/kernel"
	"github.com/mrbiz/abos/backend-core/pkg/mcp"
	"github.com/mrbiz/abos/backend-core/pkg/tickets"
	"github.com/mrbiz/abos/backend-core/pkg/vault"
)

// AIEngine is the Python AI service as seen by the API (satisfied by *grpc.AIClient).
type AIEngine interface {
	DecomposeGoal(ctx context.Context, workspaceID, goalID, description string, constraints map[string]string) (*agentv1.DecomposeGoalResponse, error)
	CompilePolicy(ctx context.Context, workspaceID, rawPolicyText string) (*agentv1.CompilePolicyResponse, error)
	RunAgentHeartbeat(ctx context.Context, workspaceID, agentID, agentRole string) (*agentv1.RunAgentHeartbeatResponse, error)
}

// Resolver holds the dependencies of every GraphQL resolver.
type Resolver struct {
	Repo    *repository.Repository
	Tickets *tickets.Service
	Broker  *events.Broker
	Policy  *kernel.PolicyEvaluator
	Gateway *mcp.Gateway
	Vault   *vault.Vault
	AI      AIEngine // may be nil: AI-backed mutations then return a clear error

	// AllowPrivateMCP permits http / loopback MCP servers (development only).
	AllowPrivateMCP bool
}

// UserError is an error whose message is safe to show to API clients.
type UserError struct {
	Code    string
	Message string
}

func (e *UserError) Error() string { return e.Message }

func userErr(code, msg string) error { return &UserError{Code: code, Message: msg} }

// PresentError turns resolver errors into client responses. Known domain errors keep their
// message; anything else (database errors, panics, ...) is logged and hidden.
func PresentError(ctx context.Context, err error) *gqlerror.Error {
	gqlErr := graphqlErrorOf(err)
	if gqlErr != nil {
		return gqlErr
	}
	var ue *UserError
	switch {
	case errors.As(err, &ue):
		return &gqlerror.Error{Message: ue.Message, Extensions: map[string]any{"code": ue.Code}}
	case errors.Is(err, auth.ErrUnauthenticated):
		return &gqlerror.Error{Message: "authentication required", Extensions: map[string]any{"code": "UNAUTHENTICATED"}}
	case errors.Is(err, auth.ErrForbidden):
		return &gqlerror.Error{Message: "forbidden", Extensions: map[string]any{"code": "FORBIDDEN"}}
	case errors.Is(err, repository.ErrNotFound):
		return &gqlerror.Error{Message: "not found", Extensions: map[string]any{"code": "NOT_FOUND"}}
	case errors.Is(err, repository.ErrStateConflict), errors.Is(err, tickets.ErrNotAwaitingApproval):
		return &gqlerror.Error{Message: "the ticket is no longer awaiting a decision; refresh and try again", Extensions: map[string]any{"code": "CONFLICT"}}
	case errors.Is(err, tickets.ErrNoExecutor):
		return &gqlerror.Error{Message: "no tool executor is configured; the ticket was left awaiting approval", Extensions: map[string]any{"code": "UNAVAILABLE"}}
	case errors.Is(err, tickets.ErrInvalidInput):
		return &gqlerror.Error{Message: err.Error(), Extensions: map[string]any{"code": "BAD_USER_INPUT"}}
	}
	log.Printf("graphql internal error: %v", err)
	return &gqlerror.Error{Message: "internal error", Extensions: map[string]any{"code": "INTERNAL"}}
}

// graphqlErrorOf passes through gqlgen's own validation/parse errors untouched.
func graphqlErrorOf(err error) *gqlerror.Error {
	var ge *gqlerror.Error
	if errors.As(err, &ge) {
		if ge.Extensions != nil {
			if _, ok := ge.Extensions["code"]; ok {
				return ge
			}
		}
		// Framework-originated errors (validation, parse, limits, disabled introspection) wrap
		// nothing; errors wrapping a resolver error (ge.Err != nil) are classified below.
		if ge.Rule != "" || ge.Err == nil {
			return ge
		}
	}
	return nil
}

// requireWorkspace checks the permission and that the caller may touch `workspaceID`.
// A workspace the caller does not belong to is reported as forbidden, never as "not found".
func requireWorkspace(ctx context.Context, workspaceID string, perm auth.Permission) (*auth.Principal, error) {
	p, err := auth.Require(ctx, perm)
	if err != nil {
		return nil, err
	}
	if workspaceID != p.WorkspaceID {
		return nil, auth.ErrForbidden
	}
	return p, nil
}

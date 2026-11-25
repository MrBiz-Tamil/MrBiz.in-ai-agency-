package repository

import (
	"context"
	"github.com/mrbiz/abos/backend-core/pkg/tenant"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
)

// DBTX is the common interface for *pgx.Conn, *pgxpool.Pool, or pgx.Tx
type DBTX interface {
	Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error)
	Query(context.Context, string, ...interface{}) (pgx.Rows, error)
	QueryRow(context.Context, string, ...interface{}) pgx.Row
}

type Repository struct {
	db DBTX
}

func NewRepository(db DBTX) *Repository {
	return &Repository{db: db}
}

// --- Workspaces ---

func (r *Repository) GetWorkspace(ctx context.Context, id string) (*Workspace, error) {
	ctx = tenant.With(ctx, id)
	query := `
		SELECT id, name, default_currency, tax_jurisdiction, daily_spend_limit_usd, created_at, updated_at
		FROM workspaces
		WHERE id = $1 LIMIT 1`

	row := r.db.QueryRow(ctx, query, id)
	var ws Workspace
	err := row.Scan(&ws.ID, &ws.Name, &ws.DefaultCurrency, &ws.TaxJurisdiction, &ws.DailySpendLimitUSD, &ws.CreatedAt, &ws.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &ws, nil
}

func (r *Repository) CreateWorkspace(
	ctx context.Context,
	name string,
	currency string,
	taxJurisdiction string,
	limit decimal.Decimal,
) (*Workspace, error) {
	ctx = tenant.System(ctx)
	query := `
		INSERT INTO workspaces (name, default_currency, tax_jurisdiction, daily_spend_limit_usd)
		VALUES ($1, $2, $3, $4)
		RETURNING id, name, default_currency, tax_jurisdiction, daily_spend_limit_usd, created_at, updated_at`

	row := r.db.QueryRow(ctx, query, name, currency, taxJurisdiction, limit)
	var ws Workspace
	err := row.Scan(&ws.ID, &ws.Name, &ws.DefaultCurrency, &ws.TaxJurisdiction, &ws.DailySpendLimitUSD, &ws.CreatedAt, &ws.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &ws, nil
}

// --- Execution Tickets ---

func (r *Repository) CreateTicket(
	ctx context.Context,
	ticket ExecutionTicket,
) (*ExecutionTicket, error) {
	ctx = tenant.With(ctx, ticket.WorkspaceID)
	query := `
		INSERT INTO execution_tickets (
			workspace_id, goal_id, assigned_agent_id, status,
			idempotency_key, priority, action_type, payload
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, workspace_id, goal_id, assigned_agent_id, status, idempotency_key,
		          priority, action_type, payload, result, conflict_diff, created_at, updated_at`

	row := r.db.QueryRow(
		ctx,
		query,
		ticket.WorkspaceID,
		ticket.GoalID,
		ticket.AssignedAgentID,
		ticket.Status,
		ticket.IdempotencyKey,
		ticket.Priority,
		ticket.ActionType,
		ticket.Payload,
	)

	var res ExecutionTicket
	err := row.Scan(
		&res.ID,
		&res.WorkspaceID,
		&res.GoalID,
		&res.AssignedAgentID,
		&res.Status,
		&res.IdempotencyKey,
		&res.Priority,
		&res.ActionType,
		&res.Payload,
		&res.Result,
		&res.ConflictDiff,
		&res.CreatedAt,
		&res.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

func (r *Repository) GetTicketByIdempotencyKey(
	ctx context.Context,
	workspaceID string,
	idempotencyKey string,
) (*ExecutionTicket, error) {
	ctx = tenant.With(ctx, workspaceID)
	query := `
		SELECT id, workspace_id, goal_id, assigned_agent_id, status, idempotency_key,
		       priority, action_type, payload, result, conflict_diff, created_at, updated_at
		FROM execution_tickets
		WHERE workspace_id = $1 AND idempotency_key = $2
		LIMIT 1`

	row := r.db.QueryRow(ctx, query, workspaceID, idempotencyKey)
	var res ExecutionTicket
	err := row.Scan(
		&res.ID,
		&res.WorkspaceID,
		&res.GoalID,
		&res.AssignedAgentID,
		&res.Status,
		&res.IdempotencyKey,
		&res.Priority,
		&res.ActionType,
		&res.Payload,
		&res.Result,
		&res.ConflictDiff,
		&res.CreatedAt,
		&res.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// --- Audit X-Ray Logs ---

func (r *Repository) CreateAuditLog(
	ctx context.Context,
	log AuditXRayLog,
) (*AuditXRayLog, error) {
	ctx = tenant.With(ctx, log.WorkspaceID)
	query := `
		INSERT INTO audit_xray_logs (
			workspace_id, ticket_id, step_number, tool_name,
			input_sanitized, output_sanitized, policy_decision,
			token_cost, latency_ms
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, workspace_id, ticket_id, step_number, tool_name,
		          input_sanitized, output_sanitized, policy_decision,
		          token_cost, latency_ms, created_at`

	row := r.db.QueryRow(
		ctx,
		query,
		log.WorkspaceID,
		log.TicketID,
		log.StepNumber,
		log.ToolName,
		log.InputSanitized,
		log.OutputSanitized,
		log.PolicyDecision,
		log.TokenCost,
		log.LatencyMs,
	)

	var res AuditXRayLog
	err := row.Scan(
		&res.ID,
		&res.WorkspaceID,
		&res.TicketID,
		&res.StepNumber,
		&res.ToolName,
		&res.InputSanitized,
		&res.OutputSanitized,
		&res.PolicyDecision,
		&res.TokenCost,
		&res.LatencyMs,
		&res.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// --- Agents ---

// GetAgentDailyBudget returns the configured daily budget (USD) and status for an agent,
// scoped to the workspace so one tenant can never read another tenant's agent.
func (r *Repository) GetAgentDailyBudget(
	ctx context.Context,
	workspaceID string,
	agentID string,
) (budget decimal.Decimal, status string, err error) {
	ctx = tenant.With(ctx, workspaceID)
	query := `
		SELECT daily_budget_usd, status
		FROM agent_definitions
		WHERE workspace_id = $1 AND id = $2
		LIMIT 1`
	err = r.db.QueryRow(ctx, query, workspaceID, agentID).Scan(&budget, &status)
	return budget, status, err
}

package repository

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/mrbiz/abos/backend-core/pkg/tenant"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
)

var (
	// ErrNotFound is returned when a row does not exist (or belongs to another workspace).
	ErrNotFound = errors.New("not found")
	// ErrStateConflict is returned when a conditional update finds the row in an unexpected state.
	ErrStateConflict = errors.New("state conflict")
	// ErrDuplicate is returned on a unique-constraint violation.
	ErrDuplicate = errors.New("duplicate")
)

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "23505": // unique_violation
			return ErrDuplicate
		case "22P02": // invalid_text_representation: e.g. a malformed UUID can match nothing
			return ErrNotFound
		}
	}
	return err
}

type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// WithTx runs fn in a transaction (commit on nil, rollback on error).
func (r *Repository) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	b, ok := r.db.(txBeginner)
	if !ok {
		return errors.New("repository connection does not support transactions")
	}
	tx, err := b.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

// ---------------------------------------------------------------------------
// Tickets
// ---------------------------------------------------------------------------

const ticketCols = `id, workspace_id, goal_id, assigned_agent_id, status, idempotency_key,
	priority, action_type, payload, result, conflict_diff, created_at, updated_at`

func scanTicket(row pgx.Row) (*ExecutionTicket, error) {
	var t ExecutionTicket
	if err := row.Scan(&t.ID, &t.WorkspaceID, &t.GoalID, &t.AssignedAgentID, &t.Status, &t.IdempotencyKey,
		&t.Priority, &t.ActionType, &t.Payload, &t.Result, &t.ConflictDiff, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &t, nil
}

func (r *Repository) collectTickets(rows pgx.Rows) ([]ExecutionTicket, error) {
	defer rows.Close()
	out := []ExecutionTicket{}
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// CreateTicketIfAbsent inserts a ticket; if one already exists for (workspace, idempotency key)
// it returns that ticket with created=false. Safe under concurrent submission.
func (r *Repository) CreateTicketIfAbsent(ctx context.Context, t ExecutionTicket) (*ExecutionTicket, bool, error) {
	ctx = tenant.With(ctx, t.WorkspaceID)
	if len(t.Payload) == 0 {
		t.Payload = json.RawMessage(`{}`)
	}
	row := r.db.QueryRow(ctx, `
		INSERT INTO execution_tickets (workspace_id, goal_id, assigned_agent_id, status, idempotency_key, priority, action_type, payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (workspace_id, idempotency_key) DO NOTHING
		RETURNING `+ticketCols,
		t.WorkspaceID, t.GoalID, t.AssignedAgentID, t.Status, t.IdempotencyKey, t.Priority, t.ActionType, t.Payload)
	created, err := scanTicket(row)
	if err == nil {
		return created, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}
	existing, err := r.GetTicketByIdempotencyKey(ctx, t.WorkspaceID, t.IdempotencyKey)
	if err != nil {
		return nil, false, mapErr(err)
	}
	return existing, false, nil
}

func (r *Repository) GetTicket(ctx context.Context, workspaceID, id string) (*ExecutionTicket, error) {
	ctx = tenant.With(ctx, workspaceID)
	return scanTicket(r.db.QueryRow(ctx, `SELECT `+ticketCols+` FROM execution_tickets WHERE workspace_id=$1 AND id=$2`, workspaceID, id))
}

// ListTickets returns newest-first tickets, optionally filtered by status.
func (r *Repository) ListTickets(ctx context.Context, workspaceID string, status *string, limit, offset int) ([]ExecutionTicket, error) {
	ctx = tenant.With(ctx, workspaceID)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := r.db.Query(ctx, `SELECT `+ticketCols+` FROM execution_tickets
		WHERE workspace_id=$1 AND ($2::text IS NULL OR status=$2)
		ORDER BY created_at DESC, id LIMIT $3 OFFSET $4`, workspaceID, status, limit, offset)
	if err != nil {
		return nil, err
	}
	return r.collectTickets(rows)
}

// ListTicketsByStatuses returns newest-first tickets whose status is in the given set.
func (r *Repository) ListTicketsByStatuses(ctx context.Context, workspaceID string, statuses []string, limit int) ([]ExecutionTicket, error) {
	ctx = tenant.With(ctx, workspaceID)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `SELECT `+ticketCols+` FROM execution_tickets
		WHERE workspace_id=$1 AND status = ANY($2) ORDER BY created_at DESC, id LIMIT $3`, workspaceID, statuses, limit)
	if err != nil {
		return nil, err
	}
	return r.collectTickets(rows)
}

func (r *Repository) CountTicketsByStatus(ctx context.Context, workspaceID string) (map[string]int, error) {
	ctx = tenant.With(ctx, workspaceID)
	rows, err := r.db.Query(ctx, `SELECT status, count(*) FROM execution_tickets WHERE workspace_id=$1 GROUP BY status`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// TransitionTicket atomically moves a ticket to `to`, but only if it currently has one of
// the `from` statuses. This is what prevents double approvals and lost updates:
// it returns ErrStateConflict if another request already changed the ticket.
func (r *Repository) TransitionTicket(ctx context.Context, workspaceID, id string, from []string, to string, result, conflict []byte) (*ExecutionTicket, error) {
	ctx = tenant.With(ctx, workspaceID)
	t, err := scanTicket(r.db.QueryRow(ctx, `
		UPDATE execution_tickets SET status=$4, result=COALESCE($5::jsonb, result), conflict_diff=COALESCE($6::jsonb, conflict_diff)
		WHERE workspace_id=$1 AND id=$2 AND status = ANY($3)
		RETURNING `+ticketCols, workspaceID, id, from, to, nullJSON(result), nullJSON(conflict)))
	if errors.Is(err, ErrNotFound) {
		// Distinguish "no such ticket" from "ticket is in another state".
		if _, gerr := r.GetTicket(ctx, workspaceID, id); gerr == nil {
			return nil, ErrStateConflict
		}
		return nil, ErrNotFound
	}
	return t, err
}

// ReplaceTicketPayload changes a ticket's payload while it is still awaiting approval.
func (r *Repository) ReplaceTicketPayload(ctx context.Context, workspaceID, id string, from []string, payload []byte) (*ExecutionTicket, error) {
	ctx = tenant.With(ctx, workspaceID)
	t, err := scanTicket(r.db.QueryRow(ctx, `
		UPDATE execution_tickets SET payload=$4
		WHERE workspace_id=$1 AND id=$2 AND status = ANY($3)
		RETURNING `+ticketCols, workspaceID, id, from, payload))
	if errors.Is(err, ErrNotFound) {
		if _, gerr := r.GetTicket(ctx, workspaceID, id); gerr == nil {
			return nil, ErrStateConflict
		}
	}
	return t, err
}

func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Agents
// ---------------------------------------------------------------------------

const agentCols = `id, workspace_id, role_name, model_tier, heartbeat_cron, daily_budget_usd,
	CASE WHEN spent_on = (now() AT TIME ZONE 'utc')::date THEN spent_today_usd ELSE 0 END, status, playbook_ref, created_at, updated_at`

func scanAgent(row pgx.Row) (*AgentDefinition, error) {
	var a AgentDefinition
	if err := row.Scan(&a.ID, &a.WorkspaceID, &a.RoleName, &a.ModelTier, &a.HeartbeatCron, &a.DailyBudget, &a.SpentToday,
		&a.Status, &a.PlaybookRef, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &a, nil
}

func (r *Repository) CreateAgent(ctx context.Context, a AgentDefinition) (*AgentDefinition, error) {
	ctx = tenant.With(ctx, a.WorkspaceID)
	if a.ModelTier == "" {
		a.ModelTier = "FLASH"
	}
	return scanAgent(r.db.QueryRow(ctx, `
		INSERT INTO agent_definitions (workspace_id, role_name, model_tier, heartbeat_cron, daily_budget_usd, playbook_ref)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING `+agentCols,
		a.WorkspaceID, a.RoleName, a.ModelTier, a.HeartbeatCron, a.DailyBudget, a.PlaybookRef))
}

func (r *Repository) ListAgents(ctx context.Context, workspaceID string) ([]AgentDefinition, error) {
	ctx = tenant.With(ctx, workspaceID)
	rows, err := r.db.Query(ctx, `SELECT `+agentCols+` FROM agent_definitions WHERE workspace_id=$1 ORDER BY created_at, id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentDefinition{}
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (r *Repository) GetAgent(ctx context.Context, workspaceID, id string) (*AgentDefinition, error) {
	ctx = tenant.With(ctx, workspaceID)
	return scanAgent(r.db.QueryRow(ctx, `SELECT `+agentCols+` FROM agent_definitions WHERE workspace_id=$1 AND id=$2`, workspaceID, id))
}

func (r *Repository) SetAgentStatus(ctx context.Context, workspaceID, id, status string) (*AgentDefinition, error) {
	ctx = tenant.With(ctx, workspaceID)
	return scanAgent(r.db.QueryRow(ctx, `UPDATE agent_definitions SET status=$3 WHERE workspace_id=$1 AND id=$2 RETURNING `+agentCols, workspaceID, id, status))
}

// SetAgentSpent mirrors the day's running spend (from Redis) for display.
func (r *Repository) SetAgentSpent(ctx context.Context, workspaceID, id string, spent decimal.Decimal) error {
	ctx = tenant.With(ctx, workspaceID)
	_, err := r.db.Exec(ctx, `UPDATE agent_definitions SET spent_today_usd=$3, spent_on=(now() AT TIME ZONE 'utc')::date WHERE workspace_id=$1 AND id=$2`, workspaceID, id, spent.Round(6))
	return err
}

func (r *Repository) SetAgentBudget(ctx context.Context, workspaceID, id string, budget decimal.Decimal) (*AgentDefinition, error) {
	ctx = tenant.With(ctx, workspaceID)
	return scanAgent(r.db.QueryRow(ctx, `UPDATE agent_definitions SET daily_budget_usd=$3 WHERE workspace_id=$1 AND id=$2 RETURNING `+agentCols, workspaceID, id, budget))
}

// ---------------------------------------------------------------------------
// Goals & milestones
// ---------------------------------------------------------------------------

type Goal struct {
	ID                 string          `json:"id"`
	WorkspaceID        string          `json:"workspace_id"`
	Title              string          `json:"title"`
	Description        string          `json:"description"`
	TargetMetric       string          `json:"target_metric"`
	TargetValue        decimal.Decimal `json:"target_value"`
	CurrentValue       decimal.Decimal `json:"current_value"`
	ProjectedMarginPct decimal.Decimal `json:"projected_margin_pct"`
	Deadline           time.Time       `json:"deadline"`
	Constraints        json.RawMessage `json:"constraints"`
	Status             string          `json:"status"`
	Milestones         []Milestone     `json:"milestones"`
	CreatedAt          time.Time       `json:"created_at"`
}

type Milestone struct {
	ID                string  `json:"id"`
	GoalID            string  `json:"goal_id"`
	StepOrder         int32   `json:"step_order"`
	Action            string  `json:"action"`
	AssignedAgentRole string  `json:"assigned_agent_role"`
	CronSchedule      *string `json:"cron_schedule,omitempty"`
	IsCompleted       bool    `json:"is_completed"`
}

const goalCols = `id, workspace_id, title, description, target_metric, target_value, current_value, projected_margin_pct, deadline, constraints, status, created_at`

func scanGoal(row pgx.Row) (*Goal, error) {
	var g Goal
	if err := row.Scan(&g.ID, &g.WorkspaceID, &g.Title, &g.Description, &g.TargetMetric, &g.TargetValue, &g.CurrentValue,
		&g.ProjectedMarginPct, &g.Deadline, &g.Constraints, &g.Status, &g.CreatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &g, nil
}

func (r *Repository) loadMilestones(ctx context.Context, workspaceID string, goals []*Goal) error {
	ctx = tenant.With(ctx, workspaceID)
	for _, g := range goals {
		rows, err := r.db.Query(ctx, `SELECT id, goal_id, step_order, action, assigned_agent_role, cron_schedule, is_completed
			FROM goal_milestones WHERE workspace_id=$1 AND goal_id=$2 ORDER BY step_order`, workspaceID, g.ID)
		if err != nil {
			return err
		}
		g.Milestones = []Milestone{}
		for rows.Next() {
			var m Milestone
			if err := rows.Scan(&m.ID, &m.GoalID, &m.StepOrder, &m.Action, &m.AssignedAgentRole, &m.CronSchedule, &m.IsCompleted); err != nil {
				rows.Close()
				return err
			}
			g.Milestones = append(g.Milestones, m)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

// CreateGoal inserts a goal and its milestones in one transaction.
func (r *Repository) CreateGoal(ctx context.Context, g Goal) (*Goal, error) {
	ctx = tenant.With(ctx, g.WorkspaceID)
	if len(g.Constraints) == 0 {
		g.Constraints = json.RawMessage(`{}`)
	}
	if g.Status == "" {
		g.Status = "DRAFT"
	}
	var out *Goal
	err := r.WithTx(ctx, func(tx pgx.Tx) error {
		created, err := scanGoal(tx.QueryRow(ctx, `
			INSERT INTO business_goals (workspace_id, title, description, target_metric, target_value, projected_margin_pct, deadline, constraints, status)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+goalCols,
			g.WorkspaceID, g.Title, g.Description, g.TargetMetric, g.TargetValue, g.ProjectedMarginPct, g.Deadline, g.Constraints, g.Status))
		if err != nil {
			return err
		}
		created.Milestones = []Milestone{}
		for _, m := range g.Milestones {
			var nm Milestone
			if err := tx.QueryRow(ctx, `
				INSERT INTO goal_milestones (workspace_id, goal_id, step_order, action, assigned_agent_role, cron_schedule)
				VALUES ($1,$2,$3,$4,$5,$6) RETURNING id, goal_id, step_order, action, assigned_agent_role, cron_schedule, is_completed`,
				g.WorkspaceID, created.ID, m.StepOrder, m.Action, m.AssignedAgentRole, m.CronSchedule).
				Scan(&nm.ID, &nm.GoalID, &nm.StepOrder, &nm.Action, &nm.AssignedAgentRole, &nm.CronSchedule, &nm.IsCompleted); err != nil {
				return mapErr(err)
			}
			created.Milestones = append(created.Milestones, nm)
		}
		out = created
		return nil
	})
	return out, err
}

func (r *Repository) GetGoal(ctx context.Context, workspaceID, id string) (*Goal, error) {
	ctx = tenant.With(ctx, workspaceID)
	g, err := scanGoal(r.db.QueryRow(ctx, `SELECT `+goalCols+` FROM business_goals WHERE workspace_id=$1 AND id=$2`, workspaceID, id))
	if err != nil {
		return nil, err
	}
	return g, r.loadMilestones(ctx, workspaceID, []*Goal{g})
}

func (r *Repository) ListActiveGoals(ctx context.Context, workspaceID string) ([]*Goal, error) {
	ctx = tenant.With(ctx, workspaceID)
	rows, err := r.db.Query(ctx, `SELECT `+goalCols+` FROM business_goals WHERE workspace_id=$1 AND status IN ('DRAFT','ACTIVE') ORDER BY deadline, id`, workspaceID)
	if err != nil {
		return nil, err
	}
	goals := []*Goal{}
	for rows.Next() {
		g, err := scanGoal(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		goals = append(goals, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return goals, r.loadMilestones(ctx, workspaceID, goals)
}

func (r *Repository) SetGoalStatus(ctx context.Context, workspaceID, id, status string) error {
	ctx = tenant.With(ctx, workspaceID)
	tag, err := r.db.Exec(ctx, `UPDATE business_goals SET status=$3 WHERE workspace_id=$1 AND id=$2`, workspaceID, id, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Repository) SetMilestoneCompleted(ctx context.Context, workspaceID, goalID string, stepOrder int32) error {
	ctx = tenant.With(ctx, workspaceID)
	_, err := r.db.Exec(ctx, `UPDATE goal_milestones SET is_completed=TRUE WHERE workspace_id=$1 AND goal_id=$2 AND step_order=$3`, workspaceID, goalID, stepOrder)
	return err
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

func (r *Repository) ListAuditLogs(ctx context.Context, workspaceID, ticketID string) ([]AuditXRayLog, error) {
	ctx = tenant.With(ctx, workspaceID)
	rows, err := r.db.Query(ctx, `
		SELECT id, workspace_id, ticket_id, step_number, tool_name, input_sanitized, output_sanitized, policy_decision, token_cost, latency_ms, created_at
		FROM audit_xray_logs WHERE workspace_id=$1 AND ticket_id=$2 ORDER BY step_number, id`, workspaceID, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditXRayLog{}
	for rows.Next() {
		var l AuditXRayLog
		if err := rows.Scan(&l.ID, &l.WorkspaceID, &l.TicketID, &l.StepNumber, &l.ToolName, &l.InputSanitized, &l.OutputSanitized,
			&l.PolicyDecision, &l.TokenCost, &l.LatencyMs, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// NextAuditStep returns the next step number for a ticket's audit trail.
func (r *Repository) NextAuditStep(ctx context.Context, workspaceID, ticketID string) (int32, error) {
	ctx = tenant.With(ctx, workspaceID)
	var n int32
	err := r.db.QueryRow(ctx, `SELECT COALESCE(MAX(step_number),0)+1 FROM audit_xray_logs WHERE workspace_id=$1 AND ticket_id=$2`, workspaceID, ticketID).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------------------
// MCP connections
// ---------------------------------------------------------------------------

const mcpCols = `id, workspace_id, server_name, transport_type, server_uri, encrypted_auth_ref, health_status, allowed_scopes, created_at, updated_at`

func scanMCP(row pgx.Row) (*MCPConnection, error) {
	var c MCPConnection
	if err := row.Scan(&c.ID, &c.WorkspaceID, &c.ServerName, &c.TransportType, &c.ServerURI, &c.EncryptedAuthRef,
		&c.HealthStatus, &c.AllowedScopes, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &c, nil
}

func (r *Repository) CreateMCPConnection(ctx context.Context, c MCPConnection) (*MCPConnection, error) {
	ctx = tenant.With(ctx, c.WorkspaceID)
	if len(c.AllowedScopes) == 0 {
		c.AllowedScopes = json.RawMessage(`[]`)
	}
	return scanMCP(r.db.QueryRow(ctx, `
		INSERT INTO mcp_connections (workspace_id, server_name, transport_type, server_uri, encrypted_auth_ref, allowed_scopes)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING `+mcpCols,
		c.WorkspaceID, c.ServerName, c.TransportType, c.ServerURI, c.EncryptedAuthRef, c.AllowedScopes))
}

func (r *Repository) ListMCPConnections(ctx context.Context, workspaceID string) ([]MCPConnection, error) {
	ctx = tenant.With(ctx, workspaceID)
	rows, err := r.db.Query(ctx, `SELECT `+mcpCols+` FROM mcp_connections WHERE workspace_id=$1 ORDER BY created_at, id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MCPConnection{}
	for rows.Next() {
		c, err := scanMCP(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (r *Repository) GetMCPConnection(ctx context.Context, workspaceID, id string) (*MCPConnection, error) {
	ctx = tenant.With(ctx, workspaceID)
	return scanMCP(r.db.QueryRow(ctx, `SELECT `+mcpCols+` FROM mcp_connections WHERE workspace_id=$1 AND id=$2`, workspaceID, id))
}

func (r *Repository) FindMCPConnectionByName(ctx context.Context, workspaceID, serverName string) (*MCPConnection, error) {
	ctx = tenant.With(ctx, workspaceID)
	return scanMCP(r.db.QueryRow(ctx, `SELECT `+mcpCols+` FROM mcp_connections WHERE workspace_id=$1 AND server_name=$2 ORDER BY created_at LIMIT 1`, workspaceID, serverName))
}

func (r *Repository) SetMCPHealth(ctx context.Context, workspaceID, id, health string) error {
	ctx = tenant.With(ctx, workspaceID)
	tag, err := r.db.Exec(ctx, `UPDATE mcp_connections SET health_status=$3 WHERE workspace_id=$1 AND id=$2`, workspaceID, id, health)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Policy rules
// ---------------------------------------------------------------------------

type PolicyRuleRow struct {
	ID                   string          `json:"id"`
	WorkspaceID          string          `json:"workspace_id"`
	RuleKey              string          `json:"rule_key"`
	TriggerEvent         string          `json:"trigger_event"`
	RawPolicyText        string          `json:"raw_policy_text"`
	CompiledAST          json.RawMessage `json:"compiled_ast"`
	AssignedApprovalRole string          `json:"assigned_approval_role"`
	Enabled              bool            `json:"enabled"`
}

const policyCols = `id, workspace_id, rule_key, trigger_event, raw_policy_text, compiled_ast, assigned_approval_role, enabled`

func scanPolicy(row pgx.Row) (*PolicyRuleRow, error) {
	var p PolicyRuleRow
	if err := row.Scan(&p.ID, &p.WorkspaceID, &p.RuleKey, &p.TriggerEvent, &p.RawPolicyText, &p.CompiledAST, &p.AssignedApprovalRole, &p.Enabled); err != nil {
		return nil, mapErr(err)
	}
	return &p, nil
}

func (r *Repository) UpsertPolicyRule(ctx context.Context, p PolicyRuleRow) (*PolicyRuleRow, error) {
	ctx = tenant.With(ctx, p.WorkspaceID)
	return scanPolicy(r.db.QueryRow(ctx, `
		INSERT INTO policy_rules (workspace_id, rule_key, trigger_event, raw_policy_text, compiled_ast, assigned_approval_role, enabled)
		VALUES ($1,$2,$3,$4,$5,$6,TRUE)
		ON CONFLICT (workspace_id, rule_key) DO UPDATE SET trigger_event=EXCLUDED.trigger_event, raw_policy_text=EXCLUDED.raw_policy_text,
			compiled_ast=EXCLUDED.compiled_ast, assigned_approval_role=EXCLUDED.assigned_approval_role, enabled=TRUE
		RETURNING `+policyCols,
		p.WorkspaceID, p.RuleKey, p.TriggerEvent, p.RawPolicyText, p.CompiledAST, p.AssignedApprovalRole))
}

// ListEnabledPolicyRules returns every enabled rule across workspaces (used to warm the kernel at startup).
func (r *Repository) ListEnabledPolicyRules(ctx context.Context) ([]PolicyRuleRow, error) {
	ctx = tenant.System(ctx)
	rows, err := r.db.Query(ctx, `SELECT `+policyCols+` FROM policy_rules WHERE enabled ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PolicyRuleRow{}
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Users & sessions
// ---------------------------------------------------------------------------

type User struct {
	ID           string `json:"id"`
	WorkspaceID  string `json:"workspace_id"`
	Email        string `json:"email"`
	DisplayName  string `json:"display_name"`
	Role         string `json:"role"`
	PasswordHash string `json:"-"`
}

func (r *Repository) CreateUser(ctx context.Context, u User) (*User, error) {
	var out User
	err := r.db.QueryRow(ctx, `
		INSERT INTO users (workspace_id, email, display_name, role, password_hash) VALUES ($1,lower($2),$3,$4,$5)
		RETURNING id, workspace_id, email, display_name, role, password_hash`,
		u.WorkspaceID, u.Email, u.DisplayName, u.Role, u.PasswordHash).
		Scan(&out.ID, &out.WorkspaceID, &out.Email, &out.DisplayName, &out.Role, &out.PasswordHash)
	if err != nil {
		return nil, mapErr(err)
	}
	return &out, nil
}

// FindUsersByEmail returns every account with this email (the same email may exist in several workspaces).
func (r *Repository) FindUsersByEmail(ctx context.Context, email string) ([]User, error) {
	rows, err := r.db.Query(ctx, `SELECT id, workspace_id, email, display_name, role, password_hash FROM users WHERE email=lower($1) ORDER BY created_at, id`, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.WorkspaceID, &u.Email, &u.DisplayName, &u.Role, &u.PasswordHash); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (r *Repository) CreateSession(ctx context.Context, tokenHash []byte, userID, workspaceID string, expires time.Time) error {
	_, err := r.db.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, workspace_id, expires_at) VALUES ($1,$2,$3,$4)`, tokenHash, userID, workspaceID, expires)
	return err
}

// GetSessionUser resolves an unexpired session to its user.
func (r *Repository) GetSessionUser(ctx context.Context, tokenHash []byte) (*User, time.Time, error) {
	var u User
	var exp time.Time
	err := r.db.QueryRow(ctx, `
		SELECT u.id, u.workspace_id, u.email, u.display_name, u.role, s.expires_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash=$1 AND s.expires_at > now()`, tokenHash).
		Scan(&u.ID, &u.WorkspaceID, &u.Email, &u.DisplayName, &u.Role, &exp)
	if err != nil {
		return nil, time.Time{}, mapErr(err)
	}
	return &u, exp, nil
}

func (r *Repository) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := r.db.Exec(ctx, `DELETE FROM sessions WHERE token_hash=$1`, tokenHash)
	return err
}

func (r *Repository) DeleteExpiredSessions(ctx context.Context) error {
	_, err := r.db.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= now()`)
	return err
}

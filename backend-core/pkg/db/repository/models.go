package repository

import (
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"
)

type Workspace struct {
	ID                 string          `json:"id"`
	Name               string          `json:"name"`
	DefaultCurrency    string          `json:"default_currency"`
	TaxJurisdiction    string          `json:"tax_jurisdiction"`
	DailySpendLimitUSD decimal.Decimal `json:"daily_spend_limit_usd"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

type MCPConnection struct {
	ID               string          `json:"id"`
	WorkspaceID      string          `json:"workspace_id"`
	ServerName       string          `json:"server_name"`
	TransportType    string          `json:"transport_type"`
	ServerURI        string          `json:"server_uri"`
	EncryptedAuthRef string          `json:"encrypted_auth_ref"`
	HealthStatus     string          `json:"health_status"`
	AllowedScopes    json.RawMessage `json:"allowed_scopes"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

type CanonicalEntity struct {
	ID               string          `json:"id"`
	WorkspaceID      string          `json:"workspace_id"`
	EntityType       string          `json:"entity_type"`
	CanonicalPayload json.RawMessage `json:"canonical_payload"`
	Version          int32           `json:"version"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

type ExternalIDMapping struct {
	ID           string    `json:"id"`
	WorkspaceID  string    `json:"workspace_id"`
	CanonicalID  string    `json:"canonical_id"`
	SystemName   string    `json:"system_name"`
	ExternalID   string    `json:"external_id"`
	LastSyncedAt time.Time `json:"last_synced_at"`
}

type AgentDefinition struct {
	ID            string          `json:"id"`
	WorkspaceID   string          `json:"workspace_id"`
	RoleName      string          `json:"role_name"`
	ModelTier     string          `json:"model_tier"`
	HeartbeatCron *string         `json:"heartbeat_cron,omitempty"`
	DailyBudget   decimal.Decimal `json:"daily_budget_usd"`
	SpentToday    decimal.Decimal `json:"spent_today_usd"`
	Status        string          `json:"status"`
	PlaybookRef   *string         `json:"playbook_ref,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

type ExecutionTicket struct {
	ID              string          `json:"id"`
	WorkspaceID     string          `json:"workspace_id"`
	GoalID          *string         `json:"goal_id,omitempty"`
	AssignedAgentID *string         `json:"assigned_agent_id,omitempty"`
	Status          string          `json:"status"`
	IdempotencyKey  string          `json:"idempotency_key"`
	Priority        int32           `json:"priority"`
	ActionType      string          `json:"action_type"`
	Payload         json.RawMessage `json:"payload"`
	Result          json.RawMessage `json:"result,omitempty"`
	ConflictDiff    json.RawMessage `json:"conflict_diff,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type AuditXRayLog struct {
	ID              int64           `json:"id"`
	WorkspaceID     string          `json:"workspace_id"`
	TicketID        string          `json:"ticket_id"`
	StepNumber      int32           `json:"step_number"`
	ToolName        *string         `json:"tool_name,omitempty"`
	InputSanitized  json.RawMessage `json:"input_sanitized,omitempty"`
	OutputSanitized json.RawMessage `json:"output_sanitized,omitempty"`
	PolicyDecision  string          `json:"policy_decision"`
	TokenCost       decimal.Decimal `json:"token_cost"`
	LatencyMs       int32           `json:"latency_ms"`
	CreatedAt       time.Time       `json:"created_at"`
}

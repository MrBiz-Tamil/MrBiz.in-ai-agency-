package graphql

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/mrbiz/abos/backend-core/pkg/db/repository"
	"github.com/mrbiz/abos/backend-core/pkg/tickets"
)

func jsonMap(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

func agentToGQL(a *repository.AgentDefinition) *AgentDefinition {
	if a == nil {
		return nil
	}
	tier := ModelTierFlash
	if a.ModelTier == "FRONTIER" {
		tier = ModelTierFrontier
	}
	return &AgentDefinition{
		ID: a.ID, RoleName: a.RoleName, ModelTier: tier, HeartbeatCron: a.HeartbeatCron,
		DailyBudgetUsd: a.DailyBudget.String(), SpentTodayUsd: a.SpentToday.String(),
		Status: a.Status, PlaybookRef: a.PlaybookRef,
	}
}

func conflictToGQL(raw json.RawMessage) *ConflictDiff {
	m := jsonMap(raw)
	if m == nil {
		return nil
	}
	str := func(k string) string { s, _ := m[k].(string); return s }
	et := EntityType(strings.ToUpper(str("entityType")))
	if !et.IsValid() {
		et = EntityTypeOrder
	}
	return &ConflictDiff{EntityType: et, Field: str("field"), SystemA: str("systemA"), ValueA: str("valueA"), SystemB: str("systemB"), ValueB: str("valueB")}
}

// ticketToGQL converts a ticket; agents (if any) are resolved through `agent`.
func (r *Resolver) ticketToGQL(ctx context.Context, t repository.ExecutionTicket) *ExecutionTicket {
	out := &ExecutionTicket{
		ID: t.ID, WorkspaceID: t.WorkspaceID, GoalID: t.GoalID, Status: TicketStatus(t.Status),
		IdempotencyKey: t.IdempotencyKey, Priority: int(t.Priority), ActionType: t.ActionType,
		Payload: jsonMap(t.Payload), Result: jsonMap(t.Result), ConflictDiff: conflictToGQL(t.ConflictDiff),
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
	if out.Payload == nil {
		out.Payload = map[string]any{}
	}
	if t.AssignedAgentID != nil {
		if a, err := r.Repo.GetAgent(ctx, t.WorkspaceID, *t.AssignedAgentID); err == nil {
			out.AssignedAgent = agentToGQL(a)
		}
	}
	return out
}

func (r *Resolver) ticketsToGQL(ctx context.Context, ts []repository.ExecutionTicket) []*ExecutionTicket {
	out := make([]*ExecutionTicket, 0, len(ts))
	for _, t := range ts {
		out = append(out, r.ticketToGQL(ctx, t))
	}
	return out
}

func goalToGQL(g *repository.Goal) *BusinessGoal {
	out := &BusinessGoal{
		ID: g.ID, Title: g.Title, TargetMetric: g.TargetMetric, TargetValue: g.TargetValue.String(),
		CurrentValue: g.CurrentValue.String(), ProjectedMarginPct: g.ProjectedMarginPct.String(),
		Deadline: g.Deadline, Status: g.Status, Milestones: make([]*Milestone, 0, len(g.Milestones)),
	}
	for _, m := range g.Milestones {
		out.Milestones = append(out.Milestones, &Milestone{ID: m.ID, StepOrder: int(m.StepOrder), Action: m.Action, AssignedAgentRole: m.AssignedAgentRole, IsCompleted: m.IsCompleted})
	}
	return out
}

func auditToGQL(l repository.AuditXRayLog) *AuditXRayLog {
	return &AuditXRayLog{
		ID: itoa(l.ID), StepNumber: int(l.StepNumber), ToolName: l.ToolName,
		InputSanitized: wrapJSON(l.InputSanitized), OutputSanitized: wrapJSON(l.OutputSanitized),
		PolicyDecision: l.PolicyDecision, TokenCost: l.TokenCost.String(), LatencyMs: int(l.LatencyMs), CreatedAt: l.CreatedAt,
	}
}

// wrapJSON exposes audit JSON as an object; scalars/arrays are wrapped under "value".
func wrapJSON(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{"value": v}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func mcpToGQL(c repository.MCPConnection) *MCPConnection {
	var scopes []string
	_ = json.Unmarshal(c.AllowedScopes, &scopes)
	if scopes == nil {
		scopes = []string{}
	}
	h := HealthStatus(c.HealthStatus)
	if !h.IsValid() {
		h = HealthStatusCritical
	}
	return &MCPConnection{ID: c.ID, ServerName: c.ServerName, TransportType: c.TransportType, ServerURI: c.ServerURI, HealthStatus: h, AllowedScopes: scopes}
}

func ticketsSubmit(workspaceID, key, action string, payload []byte, goalID *string) tickets.SubmitInput {
	return tickets.SubmitInput{WorkspaceID: workspaceID, IdempotencyKey: key, ActionType: action, Payload: payload, GoalID: goalID}
}

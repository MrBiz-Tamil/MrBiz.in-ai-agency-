// Package tickets runs the ticket lifecycle: policy gate -> (human approval) -> execution,
// with an audit trail and live events at every step.
package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mrbiz/abos/backend-core/pkg/auth"
	"github.com/mrbiz/abos/backend-core/pkg/db/repository"
	"github.com/mrbiz/abos/backend-core/pkg/events"
	"github.com/mrbiz/abos/backend-core/pkg/kernel"
)

const (
	StatusQueued          = "QUEUED"
	StatusRunning         = "RUNNING"
	StatusPendingApproval = "PENDING_APPROVAL"
	StatusConflictHold    = "CONFLICT_HOLD"
	StatusCompleted       = "COMPLETED"
	StatusFailed          = "FAILED"
)

var awaitingHuman = []string{StatusPendingApproval, StatusConflictHold}

var (
	// ErrNoExecutor means no tool executor (MCP gateway) is configured, so nothing can be run.
	ErrNoExecutor = errors.New("no tool executor is configured; the action was not executed")
	// ErrNotAwaitingApproval means the ticket is not in a state a human can decide on.
	ErrNotAwaitingApproval = errors.New("ticket is not awaiting approval")
	ErrInvalidInput        = errors.New("invalid ticket input")
)

// Repo is the persistence the service needs (satisfied by *repository.Repository).
type Repo interface {
	CreateTicketIfAbsent(ctx context.Context, t repository.ExecutionTicket) (*repository.ExecutionTicket, bool, error)
	GetTicket(ctx context.Context, workspaceID, id string) (*repository.ExecutionTicket, error)
	TransitionTicket(ctx context.Context, workspaceID, id string, from []string, to string, result, conflict []byte) (*repository.ExecutionTicket, error)
	ReplaceTicketPayload(ctx context.Context, workspaceID, id string, from []string, payload []byte) (*repository.ExecutionTicket, error)
	NextAuditStep(ctx context.Context, workspaceID, ticketID string) (int32, error)
	CreateAuditLog(ctx context.Context, l repository.AuditXRayLog) (*repository.AuditXRayLog, error)
}

// Executor runs a tool against an external system (the MCP gateway).
type Executor interface {
	Execute(ctx context.Context, workspaceID, toolName, payloadJSON string) (resultJSON string, err error)
}

// Policy decides whether an action may run unattended.
type Policy interface {
	EvaluateActionFor(workspaceID, actionType, payloadJSON string) kernel.PolicyEvaluationResult
}

// Publisher receives live events.
type Publisher interface {
	Publish(topic string, v any)
}

type Service struct {
	repo     Repo
	policy   Policy
	executor Executor // may be nil: tickets then stop at the approval/execution boundary
	events   Publisher
}

func NewService(repo Repo, policy Policy, executor Executor, pub Publisher) *Service {
	return &Service{repo: repo, policy: policy, executor: executor, events: pub}
}

type SubmitInput struct {
	WorkspaceID    string
	IdempotencyKey string
	ActionType     string
	Payload        json.RawMessage
	Priority       int32
	GoalID         *string
	AgentID        *string
}

func (in SubmitInput) validate() error {
	switch {
	case in.WorkspaceID == "":
		return fmt.Errorf("%w: workspace id required", ErrInvalidInput)
	case in.IdempotencyKey == "" || len(in.IdempotencyKey) > 255:
		return fmt.Errorf("%w: idempotency key required (max 255 chars)", ErrInvalidInput)
	case in.ActionType == "" || len(in.ActionType) > 128:
		return fmt.Errorf("%w: action type required (max 128 chars)", ErrInvalidInput)
	case len(in.Payload) > 1<<20:
		return fmt.Errorf("%w: payload too large", ErrInvalidInput)
	}
	if len(in.Payload) > 0 && !json.Valid(in.Payload) {
		return fmt.Errorf("%w: payload is not valid JSON", ErrInvalidInput)
	}
	return nil
}

func (s *Service) publish(topic string, v any) {
	if s.events != nil {
		s.events.Publish(topic, v)
	}
}

func (s *Service) emit(t *repository.ExecutionTicket) {
	s.publish(events.TicketTopic(t.WorkspaceID), *t)
	if t.Status == StatusPendingApproval || t.Status == StatusConflictHold {
		s.publish(events.ApprovalTopic(t.WorkspaceID), *t)
	}
}

type auditNote struct {
	tool, decision string
	input, output  any
	latency        time.Duration
}

func rawJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		if json.Valid([]byte(s)) {
			return json.RawMessage(s)
		}
		b, _ := json.Marshal(s)
		return b
	}
	b, _ := json.Marshal(v)
	return b
}

// audit appends a Decision X-Ray step. Failures are returned so callers can decide how loud to be.
func (s *Service) audit(ctx context.Context, t *repository.ExecutionTicket, n auditNote) {
	step, err := s.repo.NextAuditStep(ctx, t.WorkspaceID, t.ID)
	if err != nil {
		return
	}
	tool := n.tool
	l, err := s.repo.CreateAuditLog(ctx, repository.AuditXRayLog{
		WorkspaceID: t.WorkspaceID, TicketID: t.ID, StepNumber: step, ToolName: &tool,
		InputSanitized: rawJSON(n.input), OutputSanitized: rawJSON(n.output),
		PolicyDecision: n.decision, TokenCost: decimal.Zero, LatencyMs: int32(n.latency.Milliseconds()),
	})
	if err == nil {
		s.publish(events.AuditTopic(t.WorkspaceID, t.ID), *l)
	}
}

// Submit creates (or replays) a ticket and drives it through the policy gate.
// The same (workspace, idempotency key) always yields the same ticket.
func (s *Service) Submit(ctx context.Context, in SubmitInput) (*repository.ExecutionTicket, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	if len(in.Payload) == 0 {
		in.Payload = json.RawMessage(`{}`)
	}
	if in.Priority == 0 {
		in.Priority = 5
	}
	t, created, err := s.repo.CreateTicketIfAbsent(ctx, repository.ExecutionTicket{
		WorkspaceID: in.WorkspaceID, GoalID: in.GoalID, AssignedAgentID: in.AgentID, Status: StatusQueued,
		IdempotencyKey: in.IdempotencyKey, Priority: in.Priority, ActionType: in.ActionType, Payload: in.Payload,
	})
	if err != nil {
		return nil, err
	}
	if !created {
		return t, nil // replay: never process the same key twice
	}
	s.emit(t)
	return s.gate(ctx, t)
}

// gate applies the deterministic policy to a freshly queued ticket.
func (s *Service) gate(ctx context.Context, t *repository.ExecutionTicket) (*repository.ExecutionTicket, error) {
	start := time.Now()
	res := s.policy.EvaluateActionFor(t.WorkspaceID, t.ActionType, string(t.Payload))
	s.audit(ctx, t, auditNote{tool: "kernel.policy_evaluator", decision: string(res.Decision), input: t.Payload,
		output: map[string]string{"reason": res.Reason, "rule": res.TriggeredRule}, latency: time.Since(start)})

	switch res.Decision {
	case kernel.DecisionAllow:
		out, err := s.run(ctx, t, []string{StatusQueued}, "")
		if errors.Is(err, ErrNoExecutor) {
			return t, nil // allowed, but no executor yet: the ticket waits in QUEUED
		}
		return out, err
	case kernel.DecisionReject:
		out, err := s.repo.TransitionTicket(ctx, t.WorkspaceID, t.ID, []string{StatusQueued}, StatusFailed,
			rawJSON(map[string]string{"error": res.Reason}), nil)
		if err == nil {
			s.emit(out)
		}
		return out, err
	default: // SUSPEND_FOR_APPROVAL, CONFLICT_HOLD and anything unknown: a human must decide
		status := StatusPendingApproval
		if res.Decision == kernel.DecisionConflictHold {
			status = StatusConflictHold
		}
		out, err := s.repo.TransitionTicket(ctx, t.WorkspaceID, t.ID, []string{StatusQueued}, status,
			rawJSON(map[string]any{"policy": map[string]string{"rule": res.TriggeredRule, "reason": res.Reason, "assignedRole": res.AssignedRole}}),
			rawJSON(conflictDiff(res)))
		if err == nil {
			s.emit(out)
		}
		return out, err
	}
}

func conflictDiff(res kernel.PolicyEvaluationResult) map[string]string {
	limit := res.Limit
	if res.Unit != "" && res.Unit != "%" {
		limit += " " + res.Unit
	} else if res.Unit == "%" {
		limit += "%"
	}
	return map[string]string{
		"entityType": "ORDER", "field": res.Field,
		"systemA": "Requested", "valueA": res.Value,
		"systemB": "Policy Limit", "valueB": strings.TrimSpace(limit) + " (" + res.TriggeredRule + ")",
	}
}

// run claims the ticket (compare-and-set on status), executes it and records the outcome.
// `approver` is set when a human released the ticket.
func (s *Service) run(ctx context.Context, t *repository.ExecutionTicket, from []string, approver string) (*repository.ExecutionTicket, error) {
	if s.executor == nil {
		// Nothing was claimed or changed: the ticket stays exactly as it was.
		return t, ErrNoExecutor
	}
	running, err := s.repo.TransitionTicket(ctx, t.WorkspaceID, t.ID, from, StatusRunning, nil, nil)
	if err != nil {
		return nil, err // ErrStateConflict: another request already handled it
	}
	s.emit(running)

	start := time.Now()
	result, execErr := s.executor.Execute(ctx, t.WorkspaceID, t.ActionType, string(running.Payload))
	latency := time.Since(start)

	if execErr != nil {
		s.audit(ctx, running, auditNote{tool: t.ActionType, decision: "EXECUTION_FAILED", input: running.Payload, output: map[string]string{"error": execErr.Error()}, latency: latency})
		out, err := s.repo.TransitionTicket(ctx, t.WorkspaceID, t.ID, []string{StatusRunning}, StatusFailed, rawJSON(map[string]string{"error": execErr.Error()}), nil)
		if err == nil {
			s.emit(out)
		}
		return out, err
	}

	decision := "ALLOW"
	if approver != "" {
		decision = "ALLOW_APPROVED"
	}
	s.audit(ctx, running, auditNote{tool: t.ActionType, decision: decision, input: running.Payload, output: result, latency: latency})
	stored := objectJSON(result)
	if approver != "" {
		stored = withApprover(stored, approver) // lets the UI tell human-approved from touchless completions
	}
	out, err := s.repo.TransitionTicket(ctx, t.WorkspaceID, t.ID, []string{StatusRunning}, StatusCompleted, stored, nil)
	if err == nil {
		s.emit(out)
	}
	return out, err
}

func withApprover(result []byte, approver string) []byte {
	var m map[string]any
	if json.Unmarshal(result, &m) != nil || m == nil {
		m = map[string]any{}
	}
	m["approved_by"] = approver
	b, _ := json.Marshal(m)
	return b
}

// objectJSON guarantees the stored result is a JSON object.
func objectJSON(result string) []byte {
	var m map[string]any
	if json.Unmarshal([]byte(result), &m) == nil && m != nil {
		return []byte(result)
	}
	b, _ := json.Marshal(map[string]any{"value": json.RawMessage(rawJSON(result))})
	return b
}

func (s *Service) loadAwaiting(ctx context.Context, p *auth.Principal, id string) (*repository.ExecutionTicket, error) {
	t, err := s.repo.GetTicket(ctx, p.WorkspaceID, id)
	if err != nil {
		return nil, err
	}
	if t.Status != StatusPendingApproval && t.Status != StatusConflictHold {
		return nil, ErrNotAwaitingApproval
	}
	return t, nil
}

// Approve releases a held ticket for execution. The human override is audited.
func (s *Service) Approve(ctx context.Context, p *auth.Principal, id, reason string) (*repository.ExecutionTicket, error) {
	if !p.Role.Can(auth.PermDecide) {
		return nil, auth.ErrForbidden
	}
	t, err := s.loadAwaiting(ctx, p, id)
	if err != nil {
		return nil, err
	}
	// A human may override a threshold, but never a hard REJECT (e.g. a negative amount).
	if res := s.policy.EvaluateActionFor(t.WorkspaceID, t.ActionType, string(t.Payload)); res.Decision == kernel.DecisionReject {
		return nil, fmt.Errorf("%w: %s", ErrInvalidInput, res.Reason)
	}
	if s.executor == nil {
		return t, ErrNoExecutor
	}
	s.audit(ctx, t, auditNote{tool: "approval.gate", decision: "APPROVED", output: map[string]string{"by": p.Email, "role": string(p.Role), "reason": reason}})
	return s.run(ctx, t, awaitingHuman, p.Email)
}

// Reject cancels a held ticket without calling any external system.
func (s *Service) Reject(ctx context.Context, p *auth.Principal, id, reason string) (*repository.ExecutionTicket, error) {
	if !p.Role.Can(auth.PermDecide) {
		return nil, auth.ErrForbidden
	}
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("%w: a rejection reason is required", ErrInvalidInput)
	}
	t, err := s.loadAwaiting(ctx, p, id)
	if err != nil {
		return nil, err
	}
	out, err := s.repo.TransitionTicket(ctx, t.WorkspaceID, t.ID, awaitingHuman, StatusFailed,
		rawJSON(map[string]string{"rejected_by": p.Email, "reason": reason}), nil)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, out, auditNote{tool: "approval.gate", decision: "REJECTED", output: map[string]string{"by": p.Email, "role": string(p.Role), "reason": reason}})
	s.emit(out)
	return out, nil
}

// ModifyAndExecute replaces the payload of a held ticket and releases it.
func (s *Service) ModifyAndExecute(ctx context.Context, p *auth.Principal, id string, payload json.RawMessage) (*repository.ExecutionTicket, error) {
	if !p.Role.Can(auth.PermDecide) {
		return nil, auth.ErrForbidden
	}
	if len(payload) == 0 || !json.Valid(payload) || len(payload) > 1<<20 {
		return nil, fmt.Errorf("%w: modified payload must be valid JSON", ErrInvalidInput)
	}
	t, err := s.loadAwaiting(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if res := s.policy.EvaluateActionFor(t.WorkspaceID, t.ActionType, string(payload)); res.Decision == kernel.DecisionReject {
		return nil, fmt.Errorf("%w: %s", ErrInvalidInput, res.Reason)
	}
	if s.executor == nil {
		return t, ErrNoExecutor
	}
	updated, err := s.repo.ReplaceTicketPayload(ctx, t.WorkspaceID, t.ID, awaitingHuman, payload)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, updated, auditNote{tool: "approval.gate", decision: "MODIFIED_AND_APPROVED",
		input: t.Payload, output: map[string]any{"by": p.Email, "role": string(p.Role), "new_payload": json.RawMessage(payload)}})
	return s.run(ctx, updated, awaitingHuman, p.Email)
}

package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/mrbiz/abos/backend-core/pkg/db/repository"
	kernelv1 "github.com/mrbiz/abos/backend-core/pkg/gen/abos/kernel/v1"
	"github.com/mrbiz/abos/backend-core/pkg/kernel"
	"github.com/mrbiz/abos/backend-core/pkg/tickets"
)

const (
	idempotencyRetention = 7 * 24 * time.Hour
	defaultLockTTL       = 30 * time.Second
	maxKeyLen            = 255
	maxPayloadBytes      = 1 << 20 // 1 MiB
)

// ToolExecutor runs a tool call against an external system (the MCP gateway).
type ToolExecutor interface {
	Execute(ctx context.Context, workspaceID, toolName, payloadJSON string) (resultJSON string, err error)
}

// SimulatedExecutor performs no external side effects. It exists for local development
// and tests only; results are explicitly marked "simulated" so they cannot be mistaken for
// a real execution.
type SimulatedExecutor struct{}

func (SimulatedExecutor) Execute(_ context.Context, _ string, toolName, _ string) (string, error) {
	b, err := json.Marshal(map[string]interface{}{
		"status":      "success",
		"simulated":   true,
		"action":      toolName,
		"executed_at": time.Now().UTC().Format(time.RFC3339),
	})
	return string(b), err
}

// AuditEntry is a single immutable Decision X-Ray record.
type AuditEntry struct {
	WorkspaceID    string
	TicketID       string
	StepNumber     int32
	ToolName       string
	Input          string
	Output         string
	PolicyDecision string
	TokenCost      decimal.Decimal
	LatencyMs      int32
}

// AuditSink persists audit entries and returns the new log id.
type AuditSink interface {
	Record(ctx context.Context, e AuditEntry) (int64, error)
}

// MemoryAuditSink is an in-process sink for development and tests.
type MemoryAuditSink struct {
	mu      sync.Mutex
	next    int64
	Entries []AuditEntry
}

func (m *MemoryAuditSink) Record(_ context.Context, e AuditEntry) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	m.Entries = append(m.Entries, e)
	return m.next, nil
}

// BudgetLimitFunc resolves the daily budget for an agent (e.g. from agent_definitions).
type BudgetLimitFunc func(ctx context.Context, workspaceID, agentID string) (decimal.Decimal, error)

// TicketSubmitter creates tickets and drives them through the policy gate (tickets.Service).
type TicketSubmitter interface {
	Submit(ctx context.Context, in tickets.SubmitInput) (*repository.ExecutionTicket, error)
}

// BudgetEvent is reported after every budget evaluation (callers decide what to do with it:
// mirror the spend, raise 80% / 100% alerts).
type BudgetEvent struct {
	WorkspaceID string
	AgentID     string
	Evaluation  kernel.BudgetEvaluation
}

type KernelServer struct {
	kernelv1.UnimplementedKernelExecutionServiceServer
	idempotencyMgr *kernel.IdempotencyManager
	budgetBreaker  *kernel.BudgetCircuitBreaker
	policyEval     *kernel.PolicyEvaluator
	executor       ToolExecutor
	audit          AuditSink
	defaultLimit   decimal.Decimal
	limitFn        BudgetLimitFunc
	tickets        TicketSubmitter
	onBudget       func(BudgetEvent)
}

// KernelOptions are the optional collaborators of the kernel server.
type KernelOptions struct {
	Executor          ToolExecutor    // nil => ExecuteTool returns Unavailable
	Audit             AuditSink       // nil => in-memory sink
	DefaultDailyLimit decimal.Decimal // used when LimitFn is nil or fails; defaults to $10
	LimitFn           BudgetLimitFunc
	Tickets           TicketSubmitter   // nil => CreateTicket returns Unavailable
	OnBudget          func(BudgetEvent) // optional hook for 80% / 100% alerts
}

func NewKernelServer(
	idemp *kernel.IdempotencyManager,
	budget *kernel.BudgetCircuitBreaker,
	policy *kernel.PolicyEvaluator,
	opts KernelOptions,
) *KernelServer {
	if opts.Audit == nil {
		opts.Audit = &MemoryAuditSink{}
	}
	if opts.DefaultDailyLimit.IsZero() {
		opts.DefaultDailyLimit = decimal.NewFromInt(10)
	}
	return &KernelServer{
		idempotencyMgr: idemp,
		budgetBreaker:  budget,
		policyEval:     policy,
		executor:       opts.Executor,
		audit:          opts.Audit,
		defaultLimit:   opts.DefaultDailyLimit,
		limitFn:        opts.LimitFn,
		tickets:        opts.Tickets,
		onBudget:       opts.OnBudget,
	}
}

func validateWorkspace(id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return status.Error(codes.InvalidArgument, "workspace_id must be a UUID")
	}
	return nil
}

func validateKey(name, v string) error {
	if v == "" || len(v) > maxKeyLen {
		return status.Errorf(codes.InvalidArgument, "%s is required (max %d chars)", name, maxKeyLen)
	}
	return nil
}

// asJSON returns s unchanged if it is valid JSON, otherwise a JSON string literal of s.
func asJSON(s string) json.RawMessage {
	if s != "" && json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(s)
	return b
}

func (s *KernelServer) recordAudit(ctx context.Context, e AuditEntry) {
	if _, err := s.audit.Record(ctx, e); err != nil {
		// The action's outcome is already decided; surface the gap loudly for operators.
		log.Printf("AUDIT WRITE FAILED workspace=%s ticket=%s tool=%s decision=%s: %v",
			e.WorkspaceID, e.TicketID, e.ToolName, e.PolicyDecision, err)
	}
}

func (s *KernelServer) ExecuteTool(
	ctx context.Context,
	req *kernelv1.ExecuteToolRequest,
) (*kernelv1.ExecuteToolResponse, error) {
	if err := validateWorkspace(req.WorkspaceId); err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(req.TicketId); err != nil {
		return nil, status.Error(codes.InvalidArgument, "ticket_id must be a UUID (required for audit coverage)")
	}
	if err := validateKey("tool_name", req.ToolName); err != nil {
		return nil, err
	}
	if err := validateKey("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if len(req.PayloadJson) > maxPayloadBytes {
		return nil, status.Error(codes.InvalidArgument, "payload too large")
	}
	start := time.Now()

	audit := func(decision, output string) {
		s.recordAudit(ctx, AuditEntry{
			WorkspaceID:    req.WorkspaceId,
			TicketID:       req.TicketId,
			StepNumber:     1,
			ToolName:       req.ToolName,
			Input:          req.PayloadJson,
			Output:         output,
			PolicyDecision: decision,
			LatencyMs:      int32(time.Since(start).Milliseconds()),
		})
	}

	// 1. Idempotency: replay completed work, refuse concurrent duplicates.
	lock, err := s.idempotencyMgr.AcquireLock(ctx, req.WorkspaceId, req.IdempotencyKey, defaultLockTTL)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "idempotency store unavailable")
	}
	if lock.IsReplayed {
		return &kernelv1.ExecuteToolResponse{Success: true, Status: "EXECUTED", ResultJson: lock.CachedResultJSON}, nil
	}
	if !lock.Acquired {
		return nil, status.Error(codes.Aborted, "operation with this idempotency key is already in progress")
	}
	release := func() {
		rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.idempotencyMgr.ReleaseLock(rctx, req.WorkspaceId, req.IdempotencyKey)
	}

	// 2. Deterministic policy gate.
	eval := s.policyEval.EvaluateActionFor(req.WorkspaceId, req.ToolName, req.PayloadJson)
	switch eval.Decision {
	case kernel.DecisionSuspendApproval:
		release() // the same key may be re-submitted once a human approves
		audit(string(eval.Decision), eval.Reason)
		return &kernelv1.ExecuteToolResponse{Success: false, Status: "PENDING_APPROVAL", ErrorMessage: eval.Reason}, nil
	case kernel.DecisionConflictHold:
		release()
		audit(string(eval.Decision), eval.Reason)
		return &kernelv1.ExecuteToolResponse{Success: false, Status: "CONFLICT_HOLD", ErrorMessage: eval.Reason}, nil
	case kernel.DecisionReject:
		release()
		audit(string(eval.Decision), eval.Reason)
		return &kernelv1.ExecuteToolResponse{Success: false, Status: "REJECTED", ErrorMessage: eval.Reason}, nil
	case kernel.DecisionAllow:
	default: // unknown decision: fail closed
		release()
		audit("REJECT", "unknown policy decision")
		return &kernelv1.ExecuteToolResponse{Success: false, Status: "REJECTED", ErrorMessage: "unknown policy decision"}, nil
	}

	// 3. Execute through the MCP gateway.
	if s.executor == nil {
		release()
		return nil, status.Error(codes.Unavailable, "no tool executor configured")
	}
	result, execErr := s.executor.Execute(ctx, req.WorkspaceId, req.ToolName, req.PayloadJson)
	if execErr != nil {
		release() // allow a retry of the failed operation
		audit("ALLOW_EXECUTION_FAILED", execErr.Error())
		return &kernelv1.ExecuteToolResponse{Success: false, Status: "FAILED", ErrorMessage: execErr.Error()}, nil
	}

	// 4. Persist the result so replays return it instead of re-executing.
	if err := s.idempotencyMgr.StoreResult(ctx, req.WorkspaceId, req.IdempotencyKey, result, idempotencyRetention); err != nil {
		log.Printf("failed to store idempotent result for %s/%s: %v", req.WorkspaceId, req.IdempotencyKey, err)
	}
	audit(string(kernel.DecisionAllow), result)
	return &kernelv1.ExecuteToolResponse{Success: true, Status: "EXECUTED", ResultJson: result}, nil
}

func (s *KernelServer) AcquireIdempotencyLock(
	ctx context.Context,
	req *kernelv1.AcquireIdempotencyLockRequest,
) (*kernelv1.AcquireIdempotencyLockResponse, error) {
	if err := validateWorkspace(req.WorkspaceId); err != nil {
		return nil, err
	}
	if err := validateKey("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	ttl := time.Duration(req.TtlMs) * time.Millisecond
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	if ttl > time.Hour {
		ttl = time.Hour
	}

	lockRes, err := s.idempotencyMgr.AcquireLock(ctx, req.WorkspaceId, req.IdempotencyKey, ttl)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "idempotency store unavailable")
	}
	return &kernelv1.AcquireIdempotencyLockResponse{
		Acquired:         lockRes.Acquired,
		IsReplayed:       lockRes.IsReplayed,
		CachedResultJson: lockRes.CachedResultJSON,
	}, nil
}

func (s *KernelServer) EvaluateBudget(
	ctx context.Context,
	req *kernelv1.EvaluateBudgetRequest,
) (*kernelv1.EvaluateBudgetResponse, error) {
	if err := validateWorkspace(req.WorkspaceId); err != nil {
		return nil, err
	}
	if err := validateKey("agent_id", req.AgentId); err != nil {
		return nil, err
	}
	if req.EstimatedCostUsd < 0 || req.EstimatedCostUsd != req.EstimatedCostUsd { // negative or NaN
		return nil, status.Error(codes.InvalidArgument, "estimated_cost_usd must be a non-negative number")
	}

	limit := s.defaultLimit
	if s.limitFn != nil {
		l, err := s.limitFn(ctx, req.WorkspaceId, req.AgentId)
		if err != nil {
			// Fail closed: never fall back to a larger limit than the configured default.
			log.Printf("budget limit lookup failed for agent %s: %v (using default)", req.AgentId, err)
		} else {
			limit = l
		}
	}

	res, err := s.budgetBreaker.EvaluateAndIncrementBudget(
		ctx, req.WorkspaceId, req.AgentId, decimal.NewFromFloat(req.EstimatedCostUsd), limit,
	)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "budget store unavailable")
	}

	if s.onBudget != nil {
		s.onBudget(BudgetEvent{WorkspaceID: req.WorkspaceId, AgentID: req.AgentId, Evaluation: res})
	}

	spent, _ := res.SpentTodayUSD.Float64()
	lim, _ := res.DailyLimitUSD.Float64()
	return &kernelv1.EvaluateBudgetResponse{
		Allowed:         res.Allowed,
		SpentTodayUsd:   spent,
		DailyLimitUsd:   lim,
		IsCircuitBroken: res.IsCircuitBroken,
	}, nil
}

func (s *KernelServer) RecordAuditLog(
	ctx context.Context,
	req *kernelv1.RecordAuditLogRequest,
) (*kernelv1.RecordAuditLogResponse, error) {
	if err := validateWorkspace(req.WorkspaceId); err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(req.TicketId); err != nil {
		return nil, status.Error(codes.InvalidArgument, "ticket_id must be a UUID")
	}
	if req.TokenCost < 0 {
		return nil, status.Error(codes.InvalidArgument, "token_cost must be non-negative")
	}
	decision := req.PolicyDecision
	if decision == "" {
		decision = "ALLOW"
	}

	id, err := s.audit.Record(ctx, AuditEntry{
		WorkspaceID:    req.WorkspaceId,
		TicketID:       req.TicketId,
		StepNumber:     req.StepNumber,
		ToolName:       req.ToolName,
		Input:          req.InputSanitized,
		Output:         req.OutputSanitized,
		PolicyDecision: decision,
		TokenCost:      decimal.NewFromFloat(req.TokenCost),
		LatencyMs:      req.LatencyMs,
	})
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to record audit log")
	}
	return &kernelv1.RecordAuditLogResponse{Recorded: true, LogId: id}, nil
}

// CreateTicket creates (or replays) an execution ticket and runs it through the policy gate.
func (s *KernelServer) CreateTicket(
	ctx context.Context,
	req *kernelv1.CreateTicketRequest,
) (*kernelv1.CreateTicketResponse, error) {
	if s.tickets == nil {
		return nil, status.Error(codes.Unavailable, "ticket service is not configured")
	}
	if err := validateWorkspace(req.WorkspaceId); err != nil {
		return nil, err
	}
	if err := validateKey("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := validateKey("action_type", req.ActionType); err != nil {
		return nil, err
	}
	if len(req.PayloadJson) > maxPayloadBytes {
		return nil, status.Error(codes.InvalidArgument, "payload too large")
	}
	if req.Priority < 0 || req.Priority > 9 {
		return nil, status.Error(codes.InvalidArgument, "priority must be between 1 and 9")
	}
	in := tickets.SubmitInput{
		WorkspaceID: req.WorkspaceId, IdempotencyKey: req.IdempotencyKey, ActionType: req.ActionType,
		Payload: json.RawMessage(req.PayloadJson), Priority: req.Priority,
	}
	if req.AgentId != "" {
		if _, err := uuid.Parse(req.AgentId); err != nil {
			return nil, status.Error(codes.InvalidArgument, "agent_id must be a UUID")
		}
		id := req.AgentId
		in.AgentID = &id
	}
	t, err := s.tickets.Submit(ctx, in)
	switch {
	case errors.Is(err, tickets.ErrInvalidInput):
		return nil, status.Error(codes.InvalidArgument, err.Error())
	case err != nil:
		log.Printf("CreateTicket failed: %v", err)
		return nil, status.Error(codes.Internal, "failed to create ticket")
	}
	return &kernelv1.CreateTicketResponse{TicketId: t.ID, Status: t.Status}, nil
}

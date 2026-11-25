package grpc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/mrbiz/abos/backend-core/pkg/db/repository"
	kernelv1 "github.com/mrbiz/abos/backend-core/pkg/gen/abos/kernel/v1"
	"github.com/mrbiz/abos/backend-core/pkg/kernel"
	"github.com/mrbiz/abos/backend-core/pkg/tickets"
)

const (
	tWS     = "6f1c2c1e-1b0a-4d7e-9a34-0c2a5f2b7e11"
	tTicket = "0b8f3d5e-2f7a-4c1b-8d9e-1a2b3c4d5e6f"
)

type countingExecutor struct {
	calls int
	err   error
}

func (c *countingExecutor) Execute(_ context.Context, _, tool, _ string) (string, error) {
	c.calls++
	if c.err != nil {
		return "", c.err
	}
	return `{"status":"success"}`, nil
}

func newTestServer(exec ToolExecutor) (*KernelServer, *MemoryAuditSink) {
	audit := &MemoryAuditSink{}
	return NewKernelServer(
		kernel.NewIdempotencyManager(nil),
		kernel.NewBudgetCircuitBreaker(nil),
		kernel.NewPolicyEvaluator(),
		KernelOptions{Executor: exec, Audit: audit},
	), audit
}

func req(tool, key, payload string) *kernelv1.ExecuteToolRequest {
	return &kernelv1.ExecuteToolRequest{WorkspaceId: tWS, TicketId: tTicket, ToolName: tool, IdempotencyKey: key, PayloadJson: payload}
}

func code(err error) codes.Code { return status.Code(err) }

func TestExecuteTool_ExecutesOnceAndReplays(t *testing.T) {
	ex := &countingExecutor{}
	s, audit := newTestServer(ex)

	r1, err := s.ExecuteTool(context.Background(), req("inventory.sync", "k1", `{"sku":"A"}`))
	if err != nil || r1.Status != "EXECUTED" {
		t.Fatalf("expected EXECUTED, got %+v err=%v", r1, err)
	}
	r2, err := s.ExecuteTool(context.Background(), req("inventory.sync", "k1", `{"sku":"A"}`))
	if err != nil || r2.ResultJson != r1.ResultJson {
		t.Fatalf("replay must return cached result, got %+v err=%v", r2, err)
	}
	if ex.calls != 1 {
		t.Errorf("tool must execute exactly once for a repeated idempotency key, executed %d times", ex.calls)
	}
	if len(audit.Entries) != 1 {
		t.Errorf("expected 1 audit entry, got %d", len(audit.Entries))
	}
}

func TestExecuteTool_PolicyBlocksAndAudits(t *testing.T) {
	ex := &countingExecutor{}
	s, audit := newTestServer(ex)

	r, err := s.ExecuteTool(context.Background(), req("books.refund", "k2", `{"refund_amount": 5000}`))
	if err != nil || r.Status != "PENDING_APPROVAL" || r.Success {
		t.Fatalf("expected PENDING_APPROVAL, got %+v err=%v", r, err)
	}
	if ex.calls != 0 {
		t.Error("blocked action must not execute")
	}
	if len(audit.Entries) != 1 || audit.Entries[0].PolicyDecision != "SUSPEND_FOR_APPROVAL" {
		t.Errorf("blocked action must be audited: %+v", audit.Entries)
	}
	// Lock must be released so the same key can be re-submitted after approval.
	l, _ := s.AcquireIdempotencyLock(context.Background(), &kernelv1.AcquireIdempotencyLockRequest{WorkspaceId: tWS, IdempotencyKey: "k2"})
	if !l.Acquired {
		t.Error("lock should be free after a suspended action")
	}
}

func TestExecuteTool_FailureReleasesLockForRetry(t *testing.T) {
	ex := &countingExecutor{err: errors.New("erp timeout")}
	s, _ := newTestServer(ex)

	r, _ := s.ExecuteTool(context.Background(), req("inventory.sync", "k3", `{}`))
	if r.Status != "FAILED" {
		t.Fatalf("expected FAILED, got %+v", r)
	}
	ex.err = nil
	r, _ = s.ExecuteTool(context.Background(), req("inventory.sync", "k3", `{}`))
	if r.Status != "EXECUTED" {
		t.Errorf("retry after failure should execute, got %+v", r)
	}
}

func TestExecuteTool_ResultIsValidJSONEvenWithHostileToolName(t *testing.T) {
	s, _ := newTestServer(SimulatedExecutor{})
	r, err := s.ExecuteTool(context.Background(), req(`x","pwn":"1`, "k4", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.ResultJson, `"pwn":"1"`) {
		t.Errorf("tool name injected into JSON: %s", r.ResultJson)
	}
}

func TestExecuteTool_Validation(t *testing.T) {
	s, _ := newTestServer(&countingExecutor{})
	cases := map[string]*kernelv1.ExecuteToolRequest{
		"bad workspace": {WorkspaceId: "ws_1", TicketId: tTicket, ToolName: "t", IdempotencyKey: "k", PayloadJson: "{}"},
		"bad ticket":    {WorkspaceId: tWS, TicketId: "tkt", ToolName: "t", IdempotencyKey: "k", PayloadJson: "{}"},
		"no tool":       {WorkspaceId: tWS, TicketId: tTicket, IdempotencyKey: "k", PayloadJson: "{}"},
		"no key":        {WorkspaceId: tWS, TicketId: tTicket, ToolName: "t", PayloadJson: "{}"},
	}
	for name, r := range cases {
		if _, err := s.ExecuteTool(context.Background(), r); code(err) != codes.InvalidArgument {
			t.Errorf("%s: expected InvalidArgument, got %v", name, err)
		}
	}
}

func TestExecuteTool_NoExecutorFailsClosed(t *testing.T) {
	s, _ := newTestServer(nil)
	if _, err := s.ExecuteTool(context.Background(), req("inventory.sync", "k5", `{}`)); code(err) != codes.Unavailable {
		t.Errorf("without an executor the kernel must refuse, got %v", err)
	}
}

func TestEvaluateBudget_UsesLimitAndBlocks(t *testing.T) {
	s, _ := newTestServer(nil)
	ctx := context.Background()
	b := func(cost float64) *kernelv1.EvaluateBudgetResponse {
		r, err := s.EvaluateBudget(ctx, &kernelv1.EvaluateBudgetRequest{WorkspaceId: tWS, AgentId: "agt", EstimatedCostUsd: cost})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := b(9); !r.Allowed {
		t.Fatalf("9 of default $10 should be allowed: %+v", r)
	}
	if r := b(2); r.Allowed || !r.IsCircuitBroken {
		t.Errorf("exceeding the limit must trip the breaker: %+v", r)
	}
	if _, err := s.EvaluateBudget(ctx, &kernelv1.EvaluateBudgetRequest{WorkspaceId: tWS, AgentId: "agt", EstimatedCostUsd: -5}); code(err) != codes.InvalidArgument {
		t.Error("negative cost must be rejected")
	}
}

func TestRecordAuditLog(t *testing.T) {
	s, audit := newTestServer(nil)
	r, err := s.RecordAuditLog(context.Background(), &kernelv1.RecordAuditLogRequest{WorkspaceId: tWS, TicketId: tTicket, StepNumber: 2, ToolName: "t"})
	if err != nil || !r.Recorded || r.LogId == 0 || len(audit.Entries) != 1 {
		t.Fatalf("audit not recorded: %+v err=%v entries=%d", r, err, len(audit.Entries))
	}
	_, err = s.RecordAuditLog(context.Background(), &kernelv1.RecordAuditLogRequest{WorkspaceId: tWS, TicketId: "nope"})
	if code(err) != codes.InvalidArgument {
		t.Error("invalid ticket id must be rejected")
	}
}

func TestAuthToken(t *testing.T) {
	now := time.Now()
	tok := MakeAuthToken("secret", now)
	if err := VerifyAuthToken("secret", tok, now); err != nil {
		t.Errorf("valid token rejected: %v", err)
	}
	if VerifyAuthToken("other", tok, now) == nil {
		t.Error("wrong secret must be rejected")
	}
	if VerifyAuthToken("secret", tok, now.Add(10*time.Minute)) == nil {
		t.Error("expired token must be rejected")
	}
	if VerifyAuthToken("secret", "garbage", now) == nil {
		t.Error("malformed token must be rejected")
	}
}

func TestRecoveryInterceptorConvertsPanic(t *testing.T) {
	info := &grpc.UnaryServerInfo{FullMethod: "/test/Panic"}
	_, err := RecoveryUnaryInterceptor()(context.Background(), nil, info, func(context.Context, interface{}) (interface{}, error) {
		panic("boom")
	})
	if code(err) != codes.Internal {
		t.Errorf("panic should become Internal, got %v", err)
	}
}

type fakeSubmitter struct {
	got tickets.SubmitInput
	out *repository.ExecutionTicket
	err error
}

func (f *fakeSubmitter) Submit(_ context.Context, in tickets.SubmitInput) (*repository.ExecutionTicket, error) {
	f.got = in
	return f.out, f.err
}

func TestCreateTicket(t *testing.T) {
	sub := &fakeSubmitter{out: &repository.ExecutionTicket{ID: tTicket, Status: "PENDING_APPROVAL"}}
	s := NewKernelServer(kernel.NewIdempotencyManager(nil), kernel.NewBudgetCircuitBreaker(nil), kernel.NewPolicyEvaluator(), KernelOptions{Tickets: sub})
	ctx := context.Background()
	agent := "11111111-2222-4333-8444-555555555555"

	resp, err := s.CreateTicket(ctx, &kernelv1.CreateTicketRequest{WorkspaceId: tWS, AgentId: agent, IdempotencyKey: "voice_1",
		ActionType: "order.update_shipping_address", PayloadJson: `{"order_id":"o1"}`, Priority: 3})
	if err != nil || resp.TicketId != tTicket || resp.Status != "PENDING_APPROVAL" {
		t.Fatalf("create: %+v %v", resp, err)
	}
	if sub.got.WorkspaceID != tWS || sub.got.IdempotencyKey != "voice_1" || *sub.got.AgentID != agent || sub.got.Priority != 3 {
		t.Errorf("submitted input: %+v", sub.got)
	}

	bad := map[string]*kernelv1.CreateTicketRequest{
		"bad workspace": {WorkspaceId: "ws_1", IdempotencyKey: "k", ActionType: "a"},
		"no key":        {WorkspaceId: tWS, ActionType: "a"},
		"no action":     {WorkspaceId: tWS, IdempotencyKey: "k"},
		"bad agent":     {WorkspaceId: tWS, IdempotencyKey: "k", ActionType: "a", AgentId: "agt_1"},
		"bad priority":  {WorkspaceId: tWS, IdempotencyKey: "k", ActionType: "a", Priority: 42},
	}
	for name, r := range bad {
		if _, err := s.CreateTicket(ctx, r); code(err) != codes.InvalidArgument {
			t.Errorf("%s: expected InvalidArgument, got %v", name, err)
		}
	}

	sub.err = tickets.ErrInvalidInput
	if _, err := s.CreateTicket(ctx, &kernelv1.CreateTicketRequest{WorkspaceId: tWS, IdempotencyKey: "k", ActionType: "a"}); code(err) != codes.InvalidArgument {
		t.Errorf("service validation errors map to InvalidArgument, got %v", err)
	}
	sub.err = errors.New("db exploded: secret detail")
	_, err = s.CreateTicket(ctx, &kernelv1.CreateTicketRequest{WorkspaceId: tWS, IdempotencyKey: "k", ActionType: "a"})
	if code(err) != codes.Internal || strings.Contains(err.Error(), "secret") {
		t.Errorf("internal errors must be scrubbed, got %v", err)
	}

	noSvc := NewKernelServer(kernel.NewIdempotencyManager(nil), kernel.NewBudgetCircuitBreaker(nil), kernel.NewPolicyEvaluator(), KernelOptions{})
	if _, err := noSvc.CreateTicket(ctx, &kernelv1.CreateTicketRequest{WorkspaceId: tWS, IdempotencyKey: "k", ActionType: "a"}); code(err) != codes.Unavailable {
		t.Errorf("without a ticket service: %v", err)
	}
}

func TestBudgetEventsAreReported(t *testing.T) {
	var events []BudgetEvent
	s := NewKernelServer(kernel.NewIdempotencyManager(nil), kernel.NewBudgetCircuitBreaker(nil), kernel.NewPolicyEvaluator(),
		KernelOptions{OnBudget: func(e BudgetEvent) { events = append(events, e) }})
	ctx := context.Background()
	spend := func(c float64) {
		_, _ = s.EvaluateBudget(ctx, &kernelv1.EvaluateBudgetRequest{WorkspaceId: tWS, AgentId: "agt", EstimatedCostUsd: c})
	}
	spend(5) // 50%
	spend(3) // 80%: warning
	spend(5) // would exceed: breaker
	if len(events) != 3 || events[0].Evaluation.IsWarning80 || !events[1].Evaluation.IsWarning80 || !events[2].Evaluation.IsCircuitBroken {
		t.Errorf("expected an event per evaluation (none, warning, breaker), got %+v", events)
	}
}

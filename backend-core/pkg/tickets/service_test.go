package tickets_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/mrbiz/abos/backend-core/pkg/auth"
	"github.com/mrbiz/abos/backend-core/pkg/db/dbtest"
	"github.com/mrbiz/abos/backend-core/pkg/db/repository"
	"github.com/mrbiz/abos/backend-core/pkg/events"
	"github.com/mrbiz/abos/backend-core/pkg/kernel"
	"github.com/mrbiz/abos/backend-core/pkg/tickets"
)

type fakeExec struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeExec) Execute(_ context.Context, _, tool, payload string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, tool+" "+payload)
	if f.err != nil {
		return "", f.err
	}
	return `{"status":"success"}`, nil
}

type env struct {
	svc    *tickets.Service
	repo   *repository.Repository
	exec   *fakeExec
	broker *events.Broker
	ws     string
	other  string
	op     *auth.Principal
}

func setup(t *testing.T, withExecutor bool) *env {
	t.Helper()
	pool := dbtest.Pool(t)
	repo := repository.NewRepository(pool)
	ctx := context.Background()
	a, err := repo.CreateWorkspace(ctx, "a", "INR", "GST_INDIA", decimal.NewFromInt(10))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := repo.CreateWorkspace(ctx, "b", "INR", "GST_INDIA", decimal.NewFromInt(10))
	e := &env{repo: repo, exec: &fakeExec{}, broker: events.NewBroker(), ws: a.ID, other: b.ID,
		op: &auth.Principal{UserID: "u", WorkspaceID: a.ID, Email: "ops@example.com", Role: auth.RoleOperator}}
	var ex tickets.Executor
	if withExecutor {
		ex = e.exec
	}
	e.svc = tickets.NewService(repo, kernel.NewPolicyEvaluator(), ex, e.broker)
	return e
}

func submit(t *testing.T, e *env, key, action, payload string) *repository.ExecutionTicket {
	t.Helper()
	tk, err := e.svc.Submit(context.Background(), tickets.SubmitInput{WorkspaceID: e.ws, IdempotencyKey: key, ActionType: action, Payload: json.RawMessage(payload)})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return tk
}

func TestSubmit_AllowedActionRunsAndIsAudited(t *testing.T) {
	e := setup(t, true)
	tk := submit(t, e, "k1", "inventory.sync_buffer", `{"sku":"A"}`)
	if tk.Status != "COMPLETED" || len(e.exec.calls) != 1 {
		t.Fatalf("expected COMPLETED with one execution: %+v calls=%v", tk, e.exec.calls)
	}
	logs, _ := e.repo.ListAuditLogs(context.Background(), e.ws, tk.ID)
	if len(logs) != 2 || logs[0].PolicyDecision != "ALLOW" || logs[1].PolicyDecision != "ALLOW" {
		t.Errorf("expected policy + execution audit steps: %+v", logs)
	}
}

func TestSubmit_ReplayNeverExecutesTwice(t *testing.T) {
	e := setup(t, true)
	first := submit(t, e, "same", "inventory.sync_buffer", `{}`)
	second := submit(t, e, "same", "inventory.sync_buffer", `{}`)
	if first.ID != second.ID || len(e.exec.calls) != 1 {
		t.Errorf("replay must return the same ticket without re-running: %s %s calls=%d", first.ID, second.ID, len(e.exec.calls))
	}

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.svc.Submit(context.Background(), tickets.SubmitInput{WorkspaceID: e.ws, IdempotencyKey: "race", ActionType: "inventory.sync_buffer", Payload: json.RawMessage(`{}`)})
		}()
	}
	wg.Wait()
	// 1 (first) + exactly 1 for "race"
	if len(e.exec.calls) != 2 {
		t.Errorf("concurrent submissions with one key must execute once, total calls=%d", len(e.exec.calls))
	}
}

func TestSubmit_PolicyHoldsRefundAndApprovalExecutes(t *testing.T) {
	e := setup(t, true)
	tk := submit(t, e, "ref", "order.refund_requested", `{"order_id":"o1","refund_amount":"1500"}`)
	if tk.Status != "PENDING_APPROVAL" || len(e.exec.calls) != 0 {
		t.Fatalf("expected PENDING_APPROVAL and no execution: %+v", tk)
	}
	var diff map[string]string
	_ = json.Unmarshal(tk.ConflictDiff, &diff)
	if diff["field"] != "refund_amount" || diff["valueA"] != "1500" || diff["entityType"] != "ORDER" {
		t.Errorf("conflict diff: %v", diff)
	}

	ctx := context.Background()
	done, err := e.svc.Approve(ctx, e.op, tk.ID, "verified with customer")
	if err != nil || done.Status != "COMPLETED" || len(e.exec.calls) != 1 {
		t.Fatalf("approve: %+v %v calls=%d", done, err, len(e.exec.calls))
	}
	var stored map[string]any
	_ = json.Unmarshal(done.Result, &stored)
	if stored["approved_by"] != "ops@example.com" || stored["status"] != "success" {
		t.Errorf("approved result must record the approver next to the execution result: %v", stored)
	}
	// Approving again must fail and must not re-execute.
	if _, err := e.svc.Approve(ctx, e.op, tk.ID, "again"); !errors.Is(err, tickets.ErrNotAwaitingApproval) {
		t.Errorf("second approval: %v", err)
	}
	if len(e.exec.calls) != 1 {
		t.Error("double approval executed twice")
	}
	logs, _ := e.repo.ListAuditLogs(ctx, e.ws, tk.ID)
	var decisions []string
	for _, l := range logs {
		decisions = append(decisions, l.PolicyDecision)
	}
	want := []string{"SUSPEND_FOR_APPROVAL", "APPROVED", "ALLOW_APPROVED"}
	if len(decisions) != 3 || decisions[0] != want[0] || decisions[1] != want[1] || decisions[2] != want[2] {
		t.Errorf("audit decisions = %v, want %v", decisions, want)
	}
}

func TestApprove_ConcurrentApprovalsExecuteOnce(t *testing.T) {
	e := setup(t, true)
	tk := submit(t, e, "ref2", "order.refund_requested", `{"refund_amount":"9000"}`)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.svc.Approve(context.Background(), e.op, tk.ID, "x")
		}()
	}
	wg.Wait()
	if len(e.exec.calls) != 1 {
		t.Errorf("concurrent approvals must execute exactly once, got %d", len(e.exec.calls))
	}
}

func TestReject_AndAuthorization(t *testing.T) {
	e := setup(t, true)
	ctx := context.Background()
	tk := submit(t, e, "ref3", "order.refund_requested", `{"refund_amount":"5000"}`)

	auditor := &auth.Principal{WorkspaceID: e.ws, Role: auth.RoleAuditor, Email: "a@example.com"}
	if _, err := e.svc.Approve(ctx, auditor, tk.ID, "x"); !errors.Is(err, auth.ErrForbidden) {
		t.Errorf("auditor approve: %v", err)
	}
	if _, err := e.svc.Reject(ctx, auditor, tk.ID, "x"); !errors.Is(err, auth.ErrForbidden) {
		t.Errorf("auditor reject: %v", err)
	}
	foreign := &auth.Principal{WorkspaceID: e.other, Role: auth.RoleOwner, Email: "x@example.com"}
	if _, err := e.svc.Approve(ctx, foreign, tk.ID, "x"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("other workspace must not find the ticket: %v", err)
	}
	if _, err := e.svc.Reject(ctx, e.op, tk.ID, "  "); !errors.Is(err, tickets.ErrInvalidInput) {
		t.Errorf("reject needs a reason: %v", err)
	}
	out, err := e.svc.Reject(ctx, e.op, tk.ID, "customer withdrew")
	if err != nil || out.Status != "FAILED" || len(e.exec.calls) != 0 {
		t.Fatalf("reject: %+v %v", out, err)
	}
	var res map[string]string
	_ = json.Unmarshal(out.Result, &res)
	if res["rejected_by"] != "ops@example.com" || res["reason"] != "customer withdrew" {
		t.Errorf("rejection details: %v", res)
	}
}

func TestModifyAndExecute(t *testing.T) {
	e := setup(t, true)
	ctx := context.Background()
	tk := submit(t, e, "ref4", "order.refund_requested", `{"refund_amount":"5000"}`)

	if _, err := e.svc.ModifyAndExecute(ctx, e.op, tk.ID, json.RawMessage(`{"refund_amount":"-1"}`)); !errors.Is(err, tickets.ErrInvalidInput) {
		t.Errorf("a hard-rejected payload must be refused: %v", err)
	}
	if _, err := e.svc.ModifyAndExecute(ctx, e.op, tk.ID, json.RawMessage(`not json`)); !errors.Is(err, tickets.ErrInvalidInput) {
		t.Errorf("invalid json must be refused: %v", err)
	}
	out, err := e.svc.ModifyAndExecute(ctx, e.op, tk.ID, json.RawMessage(`{"refund_amount":"900"}`))
	if err != nil || out.Status != "COMPLETED" {
		t.Fatalf("modify: %+v %v", out, err)
	}
	if len(e.exec.calls) != 1 || e.exec.calls[0] != `order.refund_requested {"refund_amount": "900"}` {
		t.Errorf("executed payload must be the modified one: %v", e.exec.calls)
	}
}

func TestExecutionFailureMarksTicketFailed(t *testing.T) {
	e := setup(t, true)
	e.exec.err = errors.New("erp timeout")
	tk := submit(t, e, "f1", "inventory.sync_buffer", `{}`)
	if tk.Status != "FAILED" {
		t.Fatalf("expected FAILED, got %s", tk.Status)
	}
	var res map[string]string
	_ = json.Unmarshal(tk.Result, &res)
	if res["error"] != "erp timeout" {
		t.Errorf("failure reason must be recorded: %v", res)
	}
}

func TestNoExecutor_NothingIsClaimedOrFaked(t *testing.T) {
	e := setup(t, false)
	ctx := context.Background()
	tk := submit(t, e, "n1", "inventory.sync_buffer", `{}`)
	if tk.Status != "QUEUED" {
		t.Errorf("without an executor an allowed ticket must stay QUEUED, got %s", tk.Status)
	}
	held := submit(t, e, "n2", "order.refund_requested", `{"refund_amount":"5000"}`)
	if _, err := e.svc.Approve(ctx, e.op, held.ID, "x"); !errors.Is(err, tickets.ErrNoExecutor) {
		t.Errorf("approval without executor must report ErrNoExecutor: %v", err)
	}
	if got, _ := e.repo.GetTicket(ctx, e.ws, held.ID); got.Status != "PENDING_APPROVAL" {
		t.Errorf("ticket must remain PENDING_APPROVAL, got %s", got.Status)
	}
}

func TestEventsArePublished(t *testing.T) {
	e := setup(t, true)
	ch, cancel := e.broker.Subscribe(events.TicketTopic(e.ws))
	defer cancel()
	appr, cancelA := e.broker.Subscribe(events.ApprovalTopic(e.ws))
	defer cancelA()
	foreign, cancelF := e.broker.Subscribe(events.TicketTopic(e.other))
	defer cancelF()

	submit(t, e, "ev", "order.refund_requested", `{"refund_amount":"5000"}`)
	if len(ch) < 2 {
		t.Errorf("expected QUEUED and PENDING_APPROVAL ticket events, got %d", len(ch))
	}
	if len(appr) != 1 {
		t.Errorf("expected one approval event, got %d", len(appr))
	}
	if len(foreign) != 0 {
		t.Error("other workspace must receive nothing")
	}
}

func TestSubmitValidation(t *testing.T) {
	e := setup(t, true)
	for name, in := range map[string]tickets.SubmitInput{
		"no workspace": {IdempotencyKey: "k", ActionType: "a"},
		"no key":       {WorkspaceID: e.ws, ActionType: "a"},
		"no action":    {WorkspaceID: e.ws, IdempotencyKey: "k"},
		"bad json":     {WorkspaceID: e.ws, IdempotencyKey: "k", ActionType: "a", Payload: json.RawMessage(`{`)},
	} {
		if _, err := e.svc.Submit(context.Background(), in); !errors.Is(err, tickets.ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

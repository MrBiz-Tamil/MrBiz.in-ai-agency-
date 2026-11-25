package repository_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mrbiz/abos/backend-core/pkg/db/dbtest"
	"github.com/mrbiz/abos/backend-core/pkg/db/repository"
)

func setup(t *testing.T) (*repository.Repository, string, string) {
	t.Helper()
	pool := dbtest.Pool(t)
	repo := repository.NewRepository(pool)
	ctx := context.Background()
	a, err := repo.CreateWorkspace(ctx, "acme", "INR", "GST_INDIA", decimal.NewFromInt(100))
	if err != nil {
		t.Fatal(err)
	}
	b, err := repo.CreateWorkspace(ctx, "globex", "INR", "GST_INDIA", decimal.NewFromInt(100))
	if err != nil {
		t.Fatal(err)
	}
	return repo, a.ID, b.ID
}

func ticket(ws, key string) repository.ExecutionTicket {
	return repository.ExecutionTicket{WorkspaceID: ws, Status: "QUEUED", IdempotencyKey: key, Priority: 3, ActionType: "order.refund_requested", Payload: []byte(`{"refund_amount":"1500"}`)}
}

func TestTickets_IdempotentCreateAndWorkspaceIsolation(t *testing.T) {
	repo, a, b := setup(t)
	ctx := context.Background()

	t1, created, err := repo.CreateTicketIfAbsent(ctx, ticket(a, "k1"))
	if err != nil || !created {
		t.Fatalf("first create: created=%v err=%v", created, err)
	}
	t2, created, err := repo.CreateTicketIfAbsent(ctx, ticket(a, "k1"))
	if err != nil || created || t2.ID != t1.ID {
		t.Fatalf("same key must return the existing ticket: created=%v id=%s/%s err=%v", created, t2.ID, t1.ID, err)
	}
	// Same key in another workspace is a different ticket.
	t3, created, err := repo.CreateTicketIfAbsent(ctx, ticket(b, "k1"))
	if err != nil || !created || t3.ID == t1.ID {
		t.Fatalf("other workspace must get its own ticket: %v %v", created, err)
	}

	// Cross-workspace reads are impossible.
	if _, err := repo.GetTicket(ctx, b, t1.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("workspace B must not see workspace A's ticket, got %v", err)
	}
	listB, _ := repo.ListTickets(ctx, b, nil, 50, 0)
	if len(listB) != 1 || listB[0].ID != t3.ID {
		t.Errorf("list must be workspace scoped: %+v", listB)
	}

	// Concurrent submissions with one key produce exactly one ticket.
	var wg sync.WaitGroup
	var mu sync.Mutex
	createdCount := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, c, err := repo.CreateTicketIfAbsent(ctx, ticket(a, "race"))
			if err != nil {
				t.Errorf("race create: %v", err)
				return
			}
			if c {
				mu.Lock()
				createdCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if createdCount != 1 {
		t.Errorf("exactly one concurrent submission may create the ticket, got %d", createdCount)
	}
}

func TestTickets_TransitionIsConditional(t *testing.T) {
	repo, a, b := setup(t)
	ctx := context.Background()
	tk, _, _ := repo.CreateTicketIfAbsent(ctx, ticket(a, "k"))

	pending, err := repo.TransitionTicket(ctx, a, tk.ID, []string{"QUEUED"}, "PENDING_APPROVAL", nil, []byte(`{"field":"refund_amount"}`))
	if err != nil || pending.Status != "PENDING_APPROVAL" || string(pending.ConflictDiff) == "" {
		t.Fatalf("transition: %+v %v", pending, err)
	}
	// Double-approve race: only one wins.
	if _, err := repo.TransitionTicket(ctx, a, tk.ID, []string{"PENDING_APPROVAL"}, "RUNNING", nil, nil); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if _, err := repo.TransitionTicket(ctx, a, tk.ID, []string{"PENDING_APPROVAL"}, "RUNNING", nil, nil); !errors.Is(err, repository.ErrStateConflict) {
		t.Errorf("second claim must conflict, got %v", err)
	}
	// Another workspace cannot touch it, and learns nothing about its state.
	if _, err := repo.TransitionTicket(ctx, b, tk.ID, []string{"RUNNING"}, "COMPLETED", nil, nil); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("cross-workspace transition must be NotFound, got %v", err)
	}
	done, err := repo.TransitionTicket(ctx, a, tk.ID, []string{"RUNNING"}, "COMPLETED", []byte(`{"ok":true}`), nil)
	if err != nil || done.Status != "COMPLETED" || string(done.Result) != `{"ok": true}` {
		t.Errorf("complete: %+v %v", done, err)
	}
	// Conflict diff survives later transitions that do not set one.
	if string(done.ConflictDiff) == "" {
		t.Error("conflict_diff must be preserved")
	}

	counts, _ := repo.CountTicketsByStatus(ctx, a)
	if counts["COMPLETED"] != 1 {
		t.Errorf("counts: %v", counts)
	}
	st := "COMPLETED"
	if l, _ := repo.ListTickets(ctx, a, &st, 10, 0); len(l) != 1 {
		t.Errorf("status filter: %d", len(l))
	}
}

func TestAgents_Goals_Audit_MCP_Policy(t *testing.T) {
	repo, a, b := setup(t)
	ctx := context.Background()

	cron := "*/15 * * * *"
	ag, err := repo.CreateAgent(ctx, repository.AgentDefinition{WorkspaceID: a, RoleName: "Router", ModelTier: "FLASH", HeartbeatCron: &cron, DailyBudget: decimal.NewFromInt(5)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetAgent(ctx, b, ag.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("agents are workspace scoped: %v", err)
	}
	if ag, err = repo.SetAgentBudget(ctx, a, ag.ID, decimal.RequireFromString("7.5")); err != nil || !ag.DailyBudget.Equal(decimal.RequireFromString("7.5")) {
		t.Errorf("budget: %+v %v", ag, err)
	}
	if err := repo.SetAgentSpent(ctx, a, ag.ID, decimal.RequireFromString("3.141592")); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.GetAgent(ctx, a, ag.ID); !got.SpentToday.Equal(decimal.RequireFromString("3.141592")) {
		t.Errorf("today's spend must be visible: %s", got.SpentToday)
	}
	if err := repo.SetAgentSpent(ctx, b, ag.ID, decimal.NewFromInt(99)); err != nil {
		t.Fatal(err) // another workspace: matches no row, must not error or leak
	}
	if got, _ := repo.GetAgent(ctx, a, ag.ID); !got.SpentToday.Equal(decimal.RequireFromString("3.141592")) {
		t.Errorf("another workspace must not change this agent's spend: %s", got.SpentToday)
	}
	if ag, err = repo.SetAgentStatus(ctx, a, ag.ID, "PAUSED"); err != nil || ag.Status != "PAUSED" {
		t.Errorf("status: %+v %v", ag, err)
	}
	if bud, status, err := repo.GetAgentDailyBudget(ctx, a, ag.ID); err != nil || status != "PAUSED" || !bud.Equal(decimal.RequireFromString("7.5")) {
		t.Errorf("GetAgentDailyBudget: %v %v %v", bud, status, err)
	}

	// Goal + milestones are created atomically.
	cr := "0 23 * * *"
	g, err := repo.CreateGoal(ctx, repository.Goal{
		WorkspaceID: a, Title: "Clear stock", TargetMetric: "units", TargetValue: decimal.NewFromInt(500),
		Deadline: time.Now().Add(21 * 24 * time.Hour),
		Milestones: []repository.Milestone{
			{StepOrder: 1, Action: "inventory.sync_buffer", AssignedAgentRole: "Router", CronSchedule: &cr},
			{StepOrder: 2, Action: "books.match_payout", AssignedAgentRole: "Finance"},
		},
	})
	if err != nil || len(g.Milestones) != 2 {
		t.Fatalf("CreateGoal: %+v %v", g, err)
	}
	if _, err := repo.CreateGoal(ctx, repository.Goal{WorkspaceID: a, Title: "bad", Deadline: time.Now(),
		Milestones: []repository.Milestone{{StepOrder: 1, Action: "x", AssignedAgentRole: "r"}, {StepOrder: 1, Action: "dup", AssignedAgentRole: "r"}}}); err == nil {
		t.Error("duplicate step_order must fail")
	}
	goals, _ := repo.ListActiveGoals(ctx, a)
	if len(goals) != 1 || len(goals[0].Milestones) != 2 {
		t.Errorf("failed goal creation must roll back entirely; got %d goals", len(goals))
	}
	if gb, _ := repo.ListActiveGoals(ctx, b); len(gb) != 0 {
		t.Error("goals are workspace scoped")
	}
	if err := repo.SetMilestoneCompleted(ctx, a, g.ID, 1); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.GetGoal(ctx, a, g.ID); !got.Milestones[0].IsCompleted || got.Milestones[1].IsCompleted {
		t.Errorf("milestone completion: %+v", got.Milestones)
	}

	// Audit trail
	tk, _, _ := repo.CreateTicketIfAbsent(ctx, ticket(a, "audit"))
	for i := int32(1); i <= 3; i++ {
		step, _ := repo.NextAuditStep(ctx, a, tk.ID)
		if step != i {
			t.Errorf("NextAuditStep: want %d got %d", i, step)
		}
		tool := "kernel"
		if _, err := repo.CreateAuditLog(ctx, repository.AuditXRayLog{WorkspaceID: a, TicketID: tk.ID, StepNumber: step, ToolName: &tool, PolicyDecision: "ALLOW",
			InputSanitized: []byte(`{"in":1}`), TokenCost: decimal.RequireFromString("0.000420")}); err != nil {
			t.Fatal(err)
		}
	}
	logs, _ := repo.ListAuditLogs(ctx, a, tk.ID)
	if len(logs) != 3 || logs[0].StepNumber != 1 || !logs[2].TokenCost.Equal(decimal.RequireFromString("0.00042")) {
		t.Errorf("audit logs: %+v", logs)
	}
	if lb, _ := repo.ListAuditLogs(ctx, b, tk.ID); len(lb) != 0 {
		t.Error("audit logs are workspace scoped")
	}

	// MCP
	conn, err := repo.CreateMCPConnection(ctx, repository.MCPConnection{WorkspaceID: a, ServerName: "shopify", TransportType: "http", ServerURI: "https://x.example/mcp", EncryptedAuthRef: "ciphertext", AllowedScopes: []byte(`["shopify.get_order"]`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetMCPHealth(ctx, a, conn.ID, "DEGRADED"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetMCPHealth(ctx, b, conn.ID, "CRITICAL"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("cross-workspace health update must fail: %v", err)
	}
	if found, err := repo.FindMCPConnectionByName(ctx, a, "shopify"); err != nil || found.HealthStatus != "DEGRADED" {
		t.Errorf("find by name: %+v %v", found, err)
	}

	// Policy upsert is idempotent per (workspace, rule_key)
	p1, err := repo.UpsertPolicyRule(ctx, repository.PolicyRuleRow{WorkspaceID: a, RuleKey: "pol_1", TriggerEvent: "order.refund_requested", RawPolicyText: "v1", CompiledAST: []byte(`{"v":1}`), AssignedApprovalRole: "Finance"})
	p2, err2 := repo.UpsertPolicyRule(ctx, repository.PolicyRuleRow{WorkspaceID: a, RuleKey: "pol_1", TriggerEvent: "order.refund_requested", RawPolicyText: "v2", CompiledAST: []byte(`{"v":2}`), AssignedApprovalRole: "Finance"})
	if err != nil || err2 != nil || p1.ID != p2.ID || p2.RawPolicyText != "v2" {
		t.Errorf("upsert: %+v %+v %v %v", p1, p2, err, err2)
	}
	if rules, _ := repo.ListEnabledPolicyRules(ctx); len(rules) != 1 {
		t.Errorf("enabled rules: %d", len(rules))
	}
}

func TestUsersAndSessions(t *testing.T) {
	repo, a, _ := setup(t)
	ctx := context.Background()

	u, err := repo.CreateUser(ctx, repository.User{WorkspaceID: a, Email: "Ops@Example.com", Role: "operator", PasswordHash: "hash"})
	if err != nil || u.Email != "ops@example.com" {
		t.Fatalf("create user (email must be lower-cased): %+v %v", u, err)
	}
	if _, err := repo.CreateUser(ctx, repository.User{WorkspaceID: a, Email: "ops@example.com", Role: "operator", PasswordHash: "h"}); !errors.Is(err, repository.ErrDuplicate) {
		t.Errorf("duplicate email must be ErrDuplicate, got %v", err)
	}
	if _, err := repo.CreateUser(ctx, repository.User{WorkspaceID: a, Email: "x@example.com", Role: "superuser", PasswordHash: "h"}); err == nil {
		t.Error("invalid role must be rejected")
	}
	found, _ := repo.FindUsersByEmail(ctx, "OPS@example.com")
	if len(found) != 1 || found[0].PasswordHash != "hash" {
		t.Errorf("find users: %+v", found)
	}

	hash := []byte("0123456789abcdef0123456789abcdef")
	if err := repo.CreateSession(ctx, hash, u.ID, a, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	su, _, err := repo.GetSessionUser(ctx, hash)
	if err != nil || su.ID != u.ID || su.Role != "operator" {
		t.Fatalf("session user: %+v %v", su, err)
	}
	// Expired sessions are invisible and purgeable.
	expired := []byte("expiredexpiredexpiredexpired0000")
	_ = repo.CreateSession(ctx, expired, u.ID, a, time.Now().Add(-time.Minute))
	if _, _, err := repo.GetSessionUser(ctx, expired); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("expired session must not resolve: %v", err)
	}
	_ = repo.DeleteExpiredSessions(ctx)
	if err := repo.DeleteSession(ctx, hash); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.GetSessionUser(ctx, hash); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("deleted session must not resolve: %v", err)
	}
}

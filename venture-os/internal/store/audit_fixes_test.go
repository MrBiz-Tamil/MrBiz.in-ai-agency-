package store_test

// Regression tests for defects found by the 2026-11-25 audit of blueprint handling. Each failed before the fix.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/fixtures"
	"github.com/mrbiz/venture-os/internal/store"
)

// organizations.name is capped at 200 characters by the database. A longer name used to validate and save, then fail at
// approval with a raw constraint error that the API reported as "internal error". It is now refused when it is saved, with
// a message that names the field, which the interview can send back to the model.
func TestBusinessNameLongerThanTheColumnIsRefusedWithAMessage(t *testing.T) {
	s, _ := newStore(t)
	tn, _, err := s.CreateTenant(ctx, "u", "k")
	must(t, err)

	tooLong := fixtures.EXIMBlueprint()
	tooLong.Business.Name = strings.Repeat("N", 201)
	_, err = s.SaveBlueprint(ctx, tn, nil, tooLong, "u", "bp-long")
	if !domain.IsValidationError(err) || !strings.Contains(err.Error(), "business.name") {
		t.Fatalf("a 201-character name must be a validation problem naming the field, got: %v", err)
	}

	atLimit := fixtures.EXIMBlueprint()
	atLimit.Business.Name = strings.Repeat("N", 200)
	sb, err := s.SaveBlueprint(ctx, tn, nil, atLimit, "u", "bp-200")
	must(t, err)
	if _, err := s.ApproveBlueprint(ctx, tn, sb.ID, "founder"); err != nil {
		t.Fatalf("a 200-character name is the limit and must approve: %v", err)
	}
}

// A blueprint whose agents' skills cannot be generated is refused at submission, not after the founder approves it.
func TestBlueprintWhoseSkillsCannotBeGeneratedIsRefusedAtSave(t *testing.T) {
	s, _ := newStore(t)
	tn, _, err := s.CreateTenant(ctx, "u", "k")
	must(t, err)
	bp := fixtures.EXIMBlueprint()
	big := string(rune(0x1D54F))
	bp.Business.Description = strings.Repeat(big, domain.MaxDescriptionChars)
	bp.Business.TargetCustomers, bp.Agents[0].Responsibilities = nil, nil
	for i := 0; i < domain.MaxTargetCustomers; i++ {
		bp.Business.TargetCustomers = append(bp.Business.TargetCustomers, strings.Repeat(big, domain.MaxSentenceChars))
	}
	for i := 0; i < domain.MaxResponsibility; i++ {
		bp.Agents[0].Responsibilities = append(bp.Agents[0].Responsibilities, strings.Repeat(big, domain.MaxSentenceChars))
	}
	bp.Workflows = nil
	for i := 0; i < domain.MaxWorkflows; i++ {
		w := domain.Workflow{Key: fmt.Sprintf("wf_%02d", i), Name: strings.Repeat(big, domain.MaxLabelChars), Trigger: strings.Repeat(big, domain.MaxLabelChars)}
		for j := 0; j < domain.MaxWorkflowSteps; j++ {
			w.Steps = append(w.Steps, strings.Repeat(big, domain.MaxLabelChars))
		}
		bp.Workflows = append(bp.Workflows, w)
	}
	if err := bp.Validate(); err != nil {
		t.Skipf("fixture already exceeds a character limit: %v", err)
	}
	if _, err := s.SaveBlueprint(ctx, tn, nil, bp, "u", "bp-big"); !domain.IsValidationError(err) {
		t.Fatalf("want a validation problem, got: %v", err)
	}
}

// An approval and the audit row that says who made it must commit or fail together. The audit write used to happen after the
// decision, in its own transaction, with its error discarded: if it failed, money was approved with no record of by whom.
func TestApprovalDecisionAndItsAuditRecordCommitTogether(t *testing.T) {
	s, pool := newStore(t)
	tn, _ := approvedTenant(t, s, "founder")

	// With the audit table writable, approving writes exactly one record naming who decided.
	first := queue(t, s, tn, "PAYMENT", 100, "INR", "pay-ok")
	if _, err := s.DecideApproval(ctx, tn, first.ID, true, "sub-approver", "looks right", first.PayloadHash); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, pool, tn, `SELECT count(*)::int FROM audit_logs WHERE action='approval.approve' AND actor_id='sub-approver' AND entity_id=$1`, first.ID.String()); n != 1 {
		t.Fatalf("audit rows for the approval = %d, want 1", n)
	}

	// With the app role unable to write audit rows, approving must FAIL and leave the request pending.
	if _, err := pool.Exec(ctx, `REVOKE INSERT ON audit_logs FROM venture_app`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `GRANT INSERT ON audit_logs TO venture_app`) })
	second := queue(t, s, tn, "PAYMENT", 100, "INR", "pay-no-audit")
	if _, err := s.DecideApproval(ctx, tn, second.ID, true, "sub-approver", "", second.PayloadHash); err == nil {
		t.Fatal("an approval succeeded although its audit record could not be written")
	}
	if got, err := s.Approval(ctx, tn, second.ID); err != nil || got.Status != "PENDING_APPROVAL" {
		t.Fatalf("a failed audit write must roll the decision back: status=%v err=%v", got.Status, err)
	}
	// The same holds for a rejection.
	if _, err := s.DecideApproval(ctx, tn, second.ID, false, "sub-approver", "no", [32]byte{}); err == nil {
		t.Fatal("a rejection succeeded although its audit record could not be written")
	}

	// Once audit writes work again, the very same approval goes through.
	if _, err := pool.Exec(ctx, `GRANT INSERT ON audit_logs TO venture_app`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideApproval(ctx, tn, second.ID, true, "sub-approver", "", second.PayloadHash); err != nil {
		t.Fatalf("after the fault is gone: %v", err)
	}
}

// A spend cap change and its audit record (with what the cap was and what it became) commit or fail together.
func TestSpendCapChangeAndItsAuditRecordCommitTogether(t *testing.T) {
	s, pool := newStore(t)
	tn, _ := approvedTenant(t, s, "founder")

	must(t, s.SetSpendCap(ctx, tn, store.SpendCap{Currency: "INR", PerTransaction: i64(1_000), UpdatedBy: "sub-founder"}))
	must(t, s.SetSpendCap(ctx, tn, store.SpendCap{Currency: "INR", PerTransaction: i64(5_000), PerDay: i64(9_000), UpdatedBy: "sub-founder"}))
	raw := scalar[string](t, pool, tn, `SELECT data::text FROM audit_logs WHERE action='spend_cap.set' ORDER BY id DESC LIMIT 1`)
	var rec struct {
		Before, After struct {
			PerTransaction *int64 `json:"per_transaction"`
			PerDay         *int64 `json:"per_day"`
		}
	}
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Before.PerTransaction == nil || *rec.Before.PerTransaction != 1_000 || rec.After.PerTransaction == nil || *rec.After.PerTransaction != 5_000 || rec.After.PerDay == nil || *rec.After.PerDay != 9_000 || rec.Before.PerDay != nil {
		t.Fatalf("the record must say what the cap was and what it became: %s", raw)
	}

	if _, err := pool.Exec(ctx, `REVOKE INSERT ON audit_logs FROM venture_app`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `GRANT INSERT ON audit_logs TO venture_app`) })
	// Raising a ceiling with no record of it must not happen, and neither must removing one.
	if err := s.SetSpendCap(ctx, tn, store.SpendCap{Currency: "INR", PerTransaction: i64(900_000), UpdatedBy: "sub-founder"}); err == nil {
		t.Fatal("a cap was raised although its audit record could not be written")
	}
	if err := s.SetSpendCap(ctx, tn, store.SpendCap{Currency: "INR", UpdatedBy: "sub-founder"}); err == nil {
		t.Fatal("a cap was removed although its audit record could not be written")
	}
	caps, err := s.SpendCaps(ctx, tn)
	must(t, err)
	if len(caps) != 1 || caps[0].PerTransaction == nil || *caps[0].PerTransaction != 5_000 {
		t.Fatalf("a failed audit write must leave the cap as it was: %+v", caps)
	}
}

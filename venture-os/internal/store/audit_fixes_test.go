package store_test

// Regression tests for defects found by the 2026-11-25 audit of blueprint handling. Each failed before the fix.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/fixtures"
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

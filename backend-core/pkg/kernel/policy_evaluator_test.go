package kernel

import (
	"testing"
)

func TestPolicyEvaluator_RefundThreshold(t *testing.T) {
	evaluator := NewPolicyEvaluator()

	// Under limit (string amount) -> ALLOW
	if r := evaluator.EvaluateAction("order.refund_requested", `{"order_id": "ord_101", "refund_amount": "800.00"}`); r.Decision != DecisionAllow {
		t.Errorf("expected ALLOW for 800 refund, got %s", r.Decision)
	}

	// Over limit -> SUSPEND_FOR_APPROVAL
	r := evaluator.EvaluateAction("order.refund_requested", `{"order_id": "ord_102", "refund_amount": "1500.00"}`)
	if r.Decision != DecisionSuspendApproval {
		t.Errorf("expected SUSPEND_FOR_APPROVAL for 1500 refund, got %s", r.Decision)
	}
	if r.AssignedRole != "Store / Warehouse Manager" {
		t.Errorf("unexpected role %q", r.AssignedRole)
	}
}

func TestPolicyEvaluator_NumericAndStringBothAccepted(t *testing.T) {
	e := NewPolicyEvaluator()
	// Previously a JSON number here caused a panic (type assertion on string).
	if r := e.EvaluateAction("order.refund_requested", `{"refund_amount": 5000}`); r.Decision != DecisionSuspendApproval {
		t.Errorf("numeric refund over limit should suspend, got %s", r.Decision)
	}
	// Previously a JSON string here caused a panic (type assertion on float64).
	if r := e.EvaluateAction("order.discount_requested", `{"discount_pct": "15"}`); r.Decision != DecisionSuspendApproval {
		t.Errorf("string discount over limit should suspend, got %s", r.Decision)
	}
}

func TestPolicyEvaluator_FailsClosed(t *testing.T) {
	e := NewPolicyEvaluator()
	cases := map[string]string{
		"missing field": `{"order_id": "x"}`,
		"unparseable":   `{"refund_amount": "lots"}`,
		"wrong type":    `{"refund_amount": {"a": 1}}`,
		"null":          `{"refund_amount": null}`,
		"bool":          `{"refund_amount": true}`,
	}
	for name, payload := range cases {
		if r := e.EvaluateAction("order.refund_requested", payload); r.Decision != DecisionSuspendApproval {
			t.Errorf("%s: expected SUSPEND_FOR_APPROVAL (fail closed), got %s", name, r.Decision)
		}
	}
	if r := e.EvaluateAction("order.refund_requested", `{"refund_amount": "-5"}`); r.Decision != DecisionReject {
		t.Errorf("negative amount should be rejected, got %s", r.Decision)
	}
	if r := e.EvaluateAction("order.refund_requested", `not json`); r.Decision != DecisionReject {
		t.Errorf("malformed payload should be rejected, got %s", r.Decision)
	}
}

func TestPolicyEvaluator_MatchesToolNameVariants(t *testing.T) {
	e := NewPolicyEvaluator()
	for _, tool := range []string{"books.refund", "shopify.create_refund", "ORDER.REFUND_REQUESTED"} {
		if r := e.EvaluateAction(tool, `{"refund_amount": "99999"}`); r.Decision != DecisionSuspendApproval {
			t.Errorf("%s should be governed by the refund rule, got %s", tool, r.Decision)
		}
	}
	if r := e.EvaluateAction("inventory.get_stock", `{"sku": "A"}`); r.Decision != DecisionAllow {
		t.Errorf("ungoverned action should be allowed, got %s", r.Decision)
	}
}

func TestPolicyEvaluator_DiscountThreshold(t *testing.T) {
	e := NewPolicyEvaluator()
	if r := e.EvaluateAction("order.discount_requested", `{"discount_pct": 10.0}`); r.Decision != DecisionAllow {
		t.Errorf("expected ALLOW for 10%% discount, got %s", r.Decision)
	}
	if r := e.EvaluateAction("order.discount_requested", `{"discount_pct": 15.0}`); r.Decision != DecisionSuspendApproval {
		t.Errorf("expected SUSPEND_FOR_APPROVAL for 15%% discount, got %s", r.Decision)
	}
}

func TestPolicyEvaluator_CompiledPolicyIsWorkspaceScoped(t *testing.T) {
	e := NewPolicyEvaluator()
	ast := `{"rule_id":"pol_x","trigger_event":"order.refund_requested","conditions":[{"field":"refund_amount","operator":"GREATER_THAN","threshold":200}],"assigned_role":"Finance"}`
	if err := e.AddCompiledPolicyForWorkspace("ws-a", ast); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 500 is under the default 1000 limit but over workspace A's compiled 200 limit.
	r := e.EvaluateActionFor("ws-a", "order.refund_requested", `{"refund_amount": 500}`)
	if r.Decision != DecisionSuspendApproval || r.TriggeredRule != "pol_x" || r.AssignedRole != "Finance" {
		t.Errorf("compiled rule should trigger for its workspace, got %+v", r)
	}
	if r.Field != "refund_amount" || r.Value != "500" || r.Limit != "200" {
		t.Errorf("rule details missing: %+v", r)
	}
	// Another tenant must be unaffected.
	if r := e.EvaluateActionFor("ws-b", "order.refund_requested", `{"refund_amount": 500}`); r.Decision != DecisionAllow {
		t.Errorf("workspace B must not be affected by workspace A policy, got %+v", r)
	}

	e.RemoveRuleForWorkspace("ws-a", "pol_x")
	if r := e.EvaluateActionFor("ws-a", "order.refund_requested", `{"refund_amount": 500}`); r.Decision != DecisionAllow {
		t.Errorf("removed rule must stop applying, got %+v", r)
	}

	bad := `{"rule_id":"p","trigger_event":"x","conditions":[{"field":"f","operator":"EVAL","threshold":1}]}`
	if err := e.AddCompiledPolicyForWorkspace("ws-a", bad); err == nil {
		t.Error("unsupported operator must be rejected")
	}
	if err := e.AddCompiledPolicyForWorkspace("ws-a", `{"rule_id":"p"}`); err == nil {
		t.Error("policy without conditions must be rejected")
	}
	if err := e.AddCompiledPolicyForWorkspace("", ast); err == nil {
		t.Error("empty workspace id must be rejected")
	}
}

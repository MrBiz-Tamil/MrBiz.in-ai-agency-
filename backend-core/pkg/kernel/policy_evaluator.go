package kernel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/shopspring/decimal"
)

type PolicyDecision string

const (
	DecisionAllow           PolicyDecision = "ALLOW"
	DecisionSuspendApproval PolicyDecision = "SUSPEND_FOR_APPROVAL"
	DecisionConflictHold    PolicyDecision = "CONFLICT_HOLD"
	DecisionReject          PolicyDecision = "REJECT"
)

const defaultApprovalRole = "Store / Warehouse Manager"

// PolicyRule is a deterministic threshold rule. Each rule guards one numeric payload field.
type PolicyRule struct {
	RuleID               string          `json:"rule_id"`
	TriggerEvent         string          `json:"trigger_event"` // matched as a substring of the tool/action name
	Field                string          `json:"field"`         // payload field holding the numeric value
	Max                  decimal.Decimal `json:"max"`           // values strictly greater than Max need approval
	Unit                 string          `json:"unit,omitempty"`
	AssignedApprovalRole string          `json:"assigned_approval_role"`
}

type PolicyEvaluationResult struct {
	Decision      PolicyDecision `json:"decision"`
	TriggeredRule string         `json:"triggered_rule,omitempty"`
	Reason        string         `json:"reason,omitempty"`
	AssignedRole  string         `json:"assigned_role,omitempty"`
	// Details of the rule that fired (empty for ALLOW); used to render approval cards.
	Field string `json:"field,omitempty"`
	Value string `json:"value,omitempty"`
	Limit string `json:"limit,omitempty"`
	Unit  string `json:"unit,omitempty"`
}

// PolicyEvaluator evaluates actions against rules. It is fail-closed: if a governed action
// is missing the value a rule needs, or the value cannot be parsed, the action is
// suspended for human approval rather than allowed.
//
// Built-in default rules apply to every workspace. Rules added with AddRuleForWorkspace /
// AddCompiledPolicyForWorkspace apply ONLY to that workspace, so one tenant's policy can
// never change another tenant's behaviour.
type PolicyEvaluator struct {
	mu        sync.RWMutex
	defaults  []PolicyRule
	workspace map[string][]PolicyRule
}

func NewPolicyEvaluator() *PolicyEvaluator {
	return &PolicyEvaluator{
		workspace: make(map[string][]PolicyRule),
		defaults: []PolicyRule{
			{
				RuleID:               "rule_refund_threshold_01",
				TriggerEvent:         "refund",
				Field:                "refund_amount",
				Max:                  decimal.NewFromInt(1000), // INR 1,000
				Unit:                 "INR",
				AssignedApprovalRole: defaultApprovalRole,
			},
			{
				RuleID:               "rule_discount_threshold_01",
				TriggerEvent:         "discount",
				Field:                "discount_pct",
				Max:                  decimal.NewFromInt(12), // 12%
				Unit:                 "%",
				AssignedApprovalRole: defaultApprovalRole,
			},
		},
	}
}

// AddRuleForWorkspace registers (or replaces, by RuleID) a rule for one workspace.
func (e *PolicyEvaluator) AddRuleForWorkspace(workspaceID string, rule PolicyRule) error {
	if workspaceID == "" {
		return errors.New("workspace id is required")
	}
	if rule.RuleID == "" || rule.TriggerEvent == "" || rule.Field == "" {
		return errors.New("policy rule requires rule_id, trigger_event and field")
	}
	if rule.AssignedApprovalRole == "" {
		rule.AssignedApprovalRole = defaultApprovalRole
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rules := e.workspace[workspaceID]
	for i := range rules {
		if rules[i].RuleID == rule.RuleID {
			rules[i] = rule
			return nil
		}
	}
	e.workspace[workspaceID] = append(rules, rule)
	return nil
}

// RemoveRuleForWorkspace drops a workspace rule (e.g. when it is disabled).
func (e *PolicyEvaluator) RemoveRuleForWorkspace(workspaceID, ruleID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rules := e.workspace[workspaceID]
	for i := range rules {
		if rules[i].RuleID == ruleID {
			e.workspace[workspaceID] = append(rules[:i:i], rules[i+1:]...)
			return
		}
	}
}

// compiledAST mirrors the JSON produced by the Python policy compiler.
type compiledAST struct {
	RuleID       string `json:"rule_id"`
	TriggerEvent string `json:"trigger_event"`
	Conditions   []struct {
		Field     string      `json:"field"`
		Operator  string      `json:"operator"`
		Threshold json.Number `json:"threshold"`
	} `json:"conditions"`
	AssignedRole string `json:"assigned_role"`
}

// ParseCompiledPolicy converts the AI compiler's AST into kernel rules. Only GREATER_THAN
// threshold conditions are supported; anything else is rejected so that an LLM can never
// silently install a rule the kernel does not understand.
func ParseCompiledPolicy(astJSON string) ([]PolicyRule, error) {
	var ast compiledAST
	dec := json.NewDecoder(strings.NewReader(astJSON))
	dec.UseNumber()
	if err := dec.Decode(&ast); err != nil {
		return nil, fmt.Errorf("invalid compiled policy: %w", err)
	}
	if len(ast.Conditions) == 0 {
		return nil, errors.New("compiled policy has no conditions")
	}
	if ast.TriggerEvent == "" || ast.RuleID == "" {
		return nil, errors.New("compiled policy needs rule_id and trigger_event")
	}
	var rules []PolicyRule
	for i, c := range ast.Conditions {
		if c.Operator != "GREATER_THAN" {
			return nil, fmt.Errorf("unsupported operator %q", c.Operator)
		}
		max, err := decimal.NewFromString(c.Threshold.String())
		if err != nil || max.IsNegative() {
			return nil, fmt.Errorf("invalid threshold for field %q", c.Field)
		}
		id := ast.RuleID
		if len(ast.Conditions) > 1 {
			id = fmt.Sprintf("%s_%d", ast.RuleID, i)
		}
		rules = append(rules, PolicyRule{
			RuleID:               id,
			TriggerEvent:         ast.TriggerEvent,
			Field:                c.Field,
			Max:                  max,
			AssignedApprovalRole: ast.AssignedRole,
		})
	}
	return rules, nil
}

// AddCompiledPolicyForWorkspace parses a compiled AST and installs it for one workspace.
func (e *PolicyEvaluator) AddCompiledPolicyForWorkspace(workspaceID, astJSON string) error {
	rules, err := ParseCompiledPolicy(astJSON)
	if err != nil {
		return err
	}
	for _, r := range rules {
		if err := e.AddRuleForWorkspace(workspaceID, r); err != nil {
			return err
		}
	}
	return nil
}

// toDecimal converts a JSON value (string or number) into a decimal.
func toDecimal(v interface{}) (decimal.Decimal, bool) {
	switch t := v.(type) {
	case json.Number:
		d, err := decimal.NewFromString(t.String())
		return d, err == nil
	case string:
		d, err := decimal.NewFromString(strings.TrimSpace(t))
		return d, err == nil
	default:
		return decimal.Zero, false
	}
}

// ruleApplies reports whether a rule governs the given action name. Matching is a
// case-insensitive substring match so that "books.refund", "order.refund_requested" and
// "shopify.create_refund" are all governed by the refund rule.
func ruleApplies(rule PolicyRule, actionType string) bool {
	return strings.Contains(strings.ToLower(actionType), strings.ToLower(rule.TriggerEvent))
}

// EvaluateAction evaluates against the built-in default rules only.
func (e *PolicyEvaluator) EvaluateAction(actionType string, payloadJSON string) PolicyEvaluationResult {
	return e.EvaluateActionFor("", actionType, payloadJSON)
}

// EvaluateActionFor evaluates an action against the defaults plus the workspace's own rules.
func (e *PolicyEvaluator) EvaluateActionFor(workspaceID, actionType, payloadJSON string) PolicyEvaluationResult {
	var payload map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader([]byte(payloadJSON)))
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil || payload == nil {
		return PolicyEvaluationResult{
			Decision: DecisionReject,
			Reason:   "Malformed JSON payload",
		}
	}

	e.mu.RLock()
	rules := make([]PolicyRule, 0, len(e.defaults)+len(e.workspace[workspaceID]))
	rules = append(rules, e.defaults...)
	rules = append(rules, e.workspace[workspaceID]...)
	e.mu.RUnlock()

	for _, rule := range rules {
		if !ruleApplies(rule, actionType) {
			continue
		}

		suspend := func(reason, value string) PolicyEvaluationResult {
			return PolicyEvaluationResult{
				Decision:      DecisionSuspendApproval,
				TriggeredRule: rule.RuleID,
				Reason:        reason,
				AssignedRole:  rule.AssignedApprovalRole,
				Field:         rule.Field,
				Value:         value,
				Limit:         rule.Max.String(),
				Unit:          rule.Unit,
			}
		}

		raw, present := payload[rule.Field]
		if !present {
			return suspend(fmt.Sprintf("Required field %q missing; cannot verify against policy", rule.Field), "")
		}
		value, ok := toDecimal(raw)
		if !ok {
			return suspend(fmt.Sprintf("Field %q is not a valid number; cannot verify against policy", rule.Field), "")
		}
		if value.IsNegative() {
			return PolicyEvaluationResult{
				Decision:      DecisionReject,
				TriggeredRule: rule.RuleID,
				Reason:        fmt.Sprintf("Field %q cannot be negative", rule.Field),
			}
		}
		if value.GreaterThan(rule.Max) {
			return suspend(fmt.Sprintf("%s %s exceeds auto-approval threshold of %s%s",
				rule.Field, value.String(), rule.Max.String(), unitSuffix(rule.Unit)), value.String())
		}
	}

	return PolicyEvaluationResult{
		Decision: DecisionAllow,
		Reason:   "All deterministic governance policy checks passed",
	}
}

func unitSuffix(unit string) string {
	if unit == "" {
		return ""
	}
	if unit == "%" {
		return "%"
	}
	return " " + unit
}

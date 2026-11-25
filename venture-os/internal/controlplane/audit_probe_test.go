package controlplane_test

// Regression tests added by the 2026-11-25 backend audit: properties that were probed and found to HOLD.
// Probes for the defects the audit found live in audit_probe_bugs_test.go behind the `auditprobe` build tag.

import (
	"testing"

	"connectrpc.com/connect"

	agentv1 "github.com/mrbiz/venture-os/gen/venture/agent/v1"
	approvalv1 "github.com/mrbiz/venture-os/gen/venture/approval/v1"
)

// A user with no memberships (or only a viewer role) gets an empty inbox, not a server error.
func TestProbe_ListApprovalsForUsersWithoutApproverRoles(t *testing.T) {
	e := newEnv(t)
	goLive(t, e, "tok-founder", "probe-list")
	for _, tok := range []string{"tok-other", "tok-viewer"} {
		r, err := e.approvals(tok).ListApprovals(ctx, connect.NewRequest(&approvalv1.ListApprovalsRequest{}))
		if err != nil || len(r.Msg.Approvals) != 0 {
			t.Errorf("%s: want empty inbox, got %v %v", tok, r, err)
		}
	}
}

// Another business's approval id is invisible and untouchable whichever tenant id is supplied, for people and agents.
func TestProbe_CrossTenantApprovalAccess(t *testing.T) {
	e := newEnv(t)
	a := goLive(t, e, "tok-founder", "probe-a")
	b := goLive(t, e, "tok-other", "probe-b")
	r, err := b.agent.RequestApproval(ctx, connect.NewRequest(&agentv1.RequestApprovalRequest{
		Kind: "PAYMENT", Title: "Pay", AmountMinor: 5, Currency: "INR", IdempotencyKey: "x", PayloadJson: []byte(`{}`)}))
	if err != nil {
		t.Fatal(err)
	}
	id := r.Msg.Approval.ApprovalId
	hash := approvalHash(t, e, "tok-other", b.id, id)
	founderA := e.approvals("tok-founder")
	for _, tenant := range []string{a.id, b.id} {
		if _, err := founderA.GetApproval(ctx, connect.NewRequest(&approvalv1.GetApprovalRequest{TenantId: tenant, ApprovalId: id})); connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("GetApproval via tenant %s: %v", tenant, err)
		}
		if _, err := founderA.DecideApproval(ctx, connect.NewRequest(&approvalv1.DecideApprovalRequest{TenantId: tenant, ApprovalId: id, Approve: true, PayloadHash: hash})); connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("DecideApproval via tenant %s: %v", tenant, err)
		}
	}
	if _, err := a.agent.GetApprovalStatus(ctx, connect.NewRequest(&agentv1.GetApprovalStatusRequest{ApprovalId: id})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("agent A reading B's approval: %v", err)
	}
}

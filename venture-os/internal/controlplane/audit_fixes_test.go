package controlplane_test

// Regression tests for three defects found by the 2026-11-25 backend audit and since fixed. Each asserts the safe
// behaviour; each was first shown to fail against the unfixed code.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	agentv1 "github.com/mrbiz/venture-os/gen/venture/agent/v1"
	approvalv1 "github.com/mrbiz/venture-os/gen/venture/approval/v1"
	"github.com/mrbiz/venture-os/internal/auth"
	"github.com/mrbiz/venture-os/internal/controlplane"
	"github.com/mrbiz/venture-os/internal/crm/builtin"
	"github.com/mrbiz/venture-os/internal/fakes"
	"github.com/mrbiz/venture-os/internal/genesis"
	"github.com/mrbiz/venture-os/internal/mcptoken"
	"github.com/mrbiz/venture-os/internal/provisioner"
	"github.com/mrbiz/venture-os/internal/store"
	"github.com/mrbiz/venture-os/internal/tenancy"
	"github.com/mrbiz/venture-os/internal/testdb"
)

// newProbeEnv is newEnv with the repository wrapped, so a test can inject faults.
func newProbeEnv(t *testing.T, wrap func(controlplane.Repo) controlplane.Repo, opts ...func(*controlplane.Server)) *env {
	t.Helper()
	pool := testdb.Pool(t)
	st := store.New(pool, testdb.Role)
	hub := fakes.NewHub()
	llm := fakes.NewLLM(hub)
	signer, _ := mcptoken.NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	prov := &provisioner.Provisioner{
		Identity: fakes.NewIdentity(hub), ERP: fakes.NewERP(hub), CRM: fakes.NewCRM(hub), Support: fakes.NewSupport(hub),
		Runtime: fakes.NewRuntime(hub), Store: st, Tokens: signer,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	srv := &controlplane.Server{CRM: builtin.New(pool, tenancy.Options{AssumeRole: testdb.Role}), Repo: wrap(st),
		Interviewer: &genesis.Engine{LLM: llm, Model: "m"}, Provisioner: prov, AgentSigner: signer,
		Now: func() time.Time { return time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC) }}
	for _, o := range opts {
		o(srv)
	}
	ts := httptest.NewServer(srv.Handler(auth.StaticVerifier{
		"tok-founder": "sub-founder", "tok-other": "sub-other", "tok-viewer": "sub-viewer", "tok-approver": "sub-approver",
	}))
	t.Cleanup(ts.Close)
	return &env{url: ts.URL, hub: hub, llm: llm, store: st, signer: signer, pool: pool}
}

// claimFault fails ClaimExecution while armed, like a database blip or a dropped client connection between the
// approval being committed and its side effect starting.
type claimFault struct {
	controlplane.Repo
	armed      *atomic.Bool // fail ClaimExecution
	finishArms *atomic.Bool // fail FinishExecution (the work happened but its outcome was not recorded)
}

func (c *claimFault) FinishExecution(ctx context.Context, tenant, id uuid.UUID, ok bool, result json.RawMessage) error {
	if c.finishArms != nil && c.finishArms.Load() {
		return errors.New("simulated database blip")
	}
	return c.Repo.FinishExecution(ctx, tenant, id, ok, result)
}

func (c *claimFault) ClaimExecution(ctx context.Context, tenant, id uuid.UUID) (bool, error) {
	if c.armed.Load() {
		return false, errors.New("simulated database blip")
	}
	return c.Repo.ClaimExecution(ctx, tenant, id)
}

// An approved invoice whose execution failed to start can be finished by approving again.
func TestFix_ApprovedButUnexecutedInvoiceHasARecoveryPath(t *testing.T) {
	armed := &atomic.Bool{}
	e := newProbeEnv(t, func(r controlplane.Repo) controlplane.Repo { return &claimFault{Repo: r, armed: armed} })
	b := goLive(t, e, "tok-founder", "probe-claim")
	inv, err := b.agent.DraftInvoice(ctx, connect.NewRequest(&agentv1.DraftInvoiceRequest{Draft: draft("Berlin GmbH", "i-1", "GST5",
		&agentv1.DocumentLine{Description: "Black pepper", Quantity: 10, UnitPriceMinor: 1000})}))
	if err != nil {
		t.Fatal(err)
	}
	id := inv.Msg.Document.ApprovalId
	hash := approvalHash(t, e, "tok-founder", b.id, id)
	founder := e.approvals("tok-founder")

	armed.Store(true)
	first, err := founder.DecideApproval(ctx, connect.NewRequest(&approvalv1.DecideApprovalRequest{TenantId: b.id, ApprovalId: id, Approve: true, PayloadHash: hash}))
	if err != nil {
		t.Fatalf("first decide: %v", err)
	}
	t.Logf("first decide: status=%s executed=%v err=%q", first.Msg.Status, first.Msg.Executed, first.Msg.ExecutionError)
	armed.Store(false)

	second, err := founder.DecideApproval(ctx, connect.NewRequest(&approvalv1.DecideApprovalRequest{TenantId: b.id, ApprovalId: id, Approve: true, PayloadHash: hash}))
	if err != nil {
		t.Fatalf("retry refused (%v): approved but never executed, and nothing else calls the executor", err)
	}
	if !second.Msg.Executed {
		t.Fatalf("retry did not execute: status=%s", second.Msg.Status)
	}
}

// The spend cap cannot be avoided by naming a currency that has no cap row.
func TestFix_SpendCapCannotBeSidesteppedWithAnotherCurrency(t *testing.T) {
	e := newEnv(t)
	b := goLive(t, e, "tok-founder", "probe-cap")
	hundred := int64(100)
	if err := e.store.SetSpendCap(ctx, b.uuid, store.SpendCap{Currency: "INR", PerTransaction: &hundred, UpdatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	founder := e.approvals("tok-founder")
	pay := func(key, currency string) error {
		r, err := b.agent.RequestApproval(ctx, connect.NewRequest(&agentv1.RequestApprovalRequest{
			Kind: "PAYMENT", Title: "Pay supplier", AmountMinor: 1_000_000_00, Currency: currency, IdempotencyKey: key, PayloadJson: []byte(`{}`)}))
		if err != nil {
			return err
		}
		id := r.Msg.Approval.ApprovalId
		_, err = founder.DecideApproval(ctx, connect.NewRequest(&approvalv1.DecideApprovalRequest{
			TenantId: b.id, ApprovalId: id, Approve: true, PayloadHash: approvalHash(t, e, "tok-founder", b.id, id)}))
		return err
	}
	if err := pay("p-inr", "INR"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("sanity: an INR payment over the cap must be blocked, got %v", err)
	}
	if err := pay("p-zzz", "ZZZ"); err == nil {
		t.Fatal("a 1,000,000.00 PAYMENT in currency ZZZ was approved although every payment is capped at 1.00 INR")
	}
}

type repeatByte byte

func (r repeatByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

// Request messages are size-limited, so a huge unauthenticated body is refused before it is parsed.
func TestFix_HugeUnauthenticatedBodyIsRefusedBeforeParsing(t *testing.T) {
	e := newEnv(t)
	const size = 48 << 20
	body := io.MultiReader(strings.NewReader(`{"idempotencyKey":"`), io.LimitReader(repeatByte('a'), size), strings.NewReader(`"}  not-json`))
	req, _ := http.NewRequest(http.MethodPost, e.url+"/venture.tenant.v1.TenantService/CreateBusiness", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("connection closed early (good): %v", err)
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	t.Logf("status=%d body=%s", resp.StatusCode, bytes.TrimSpace(raw)[:min(len(raw), 200)])
	if !strings.Contains(string(raw), "resource_exhausted") {
		t.Fatalf("a %d MiB request was not refused as too large: status %d", size>>20, resp.StatusCode)
	}
}

// An execution that ran but could not record its outcome stays EXECUTING; once its lease expires, approving again takes
// it over and finishes it, and the ledger is posted exactly once.
func TestFix_StaleExecutingClaimCanBeTakenOver(t *testing.T) {
	finish := &atomic.Bool{}
	e := newProbeEnv(t, func(r controlplane.Repo) controlplane.Repo {
		return &claimFault{Repo: r, armed: &atomic.Bool{}, finishArms: finish}
	})
	b := goLive(t, e, "tok-founder", "probe-lease")
	inv, err := b.agent.DraftInvoice(ctx, connect.NewRequest(&agentv1.DraftInvoiceRequest{Draft: draft("Berlin GmbH", "i-1", "GST5",
		&agentv1.DocumentLine{Description: "Black pepper", Quantity: 10, UnitPriceMinor: 1000})}))
	if err != nil {
		t.Fatal(err)
	}
	id := inv.Msg.Document.ApprovalId
	req := &approvalv1.DecideApprovalRequest{TenantId: b.id, ApprovalId: id, Approve: true, PayloadHash: approvalHash(t, e, "tok-founder", b.id, id)}
	founder := e.approvals("tok-founder")

	finish.Store(true)
	first, err := founder.DecideApproval(ctx, connect.NewRequest(req))
	if err != nil || first.Msg.Status != "EXECUTING" {
		t.Fatalf("first decide: %v %v", first, err)
	}
	finish.Store(false)

	// Inside the lease another run must not start.
	if again, err := founder.DecideApproval(ctx, connect.NewRequest(req)); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("inside the lease a repeat must be refused as a duplicate: %v %v", again, err)
	}

	// Age the claim past the lease (the guard trigger always stamps updated_at, so pause it for the backdating).
	for _, q := range []string{
		`ALTER TABLE approval_queue DISABLE TRIGGER approval_queue_guard`,
		`UPDATE approval_queue SET updated_at = now() - interval '10 minutes' WHERE id = '` + id + `'`,
		`ALTER TABLE approval_queue ENABLE TRIGGER approval_queue_guard`,
	} {
		if _, err := e.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	done, err := founder.DecideApproval(ctx, connect.NewRequest(req))
	if err != nil || !done.Msg.Executed || done.Msg.Status != "EXECUTED" {
		t.Fatalf("after the lease: %v %v", done, err)
	}
	var journals int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_journals WHERE source_id = $1`, id).Scan(&journals); err != nil || journals != 1 {
		t.Fatalf("journals for the invoice = %d (%v), want exactly 1", journals, err)
	}
	// And now it is final: a further approve is refused.
	if _, err := founder.DecideApproval(ctx, connect.NewRequest(req)); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("approve after EXECUTED: %v", err)
	}
}

// The new cap rule must not block a business that has set no caps at all, or a payment in a currency that has a cap.
func TestFix_CapRuleDoesNotBlockBusinessesWithoutCaps(t *testing.T) {
	e := newEnv(t)
	b := goLive(t, e, "tok-founder", "probe-nocap")
	founder := e.approvals("tok-founder")
	decide := func(key, currency string) error {
		r, err := b.agent.RequestApproval(ctx, connect.NewRequest(&agentv1.RequestApprovalRequest{
			Kind: "PAYMENT", Title: "Pay", AmountMinor: 5000, Currency: currency, IdempotencyKey: key, PayloadJson: []byte(`{}`)}))
		if err != nil {
			return err
		}
		aid := r.Msg.Approval.ApprovalId
		_, err = founder.DecideApproval(ctx, connect.NewRequest(&approvalv1.DecideApprovalRequest{
			TenantId: b.id, ApprovalId: aid, Approve: true, PayloadHash: approvalHash(t, e, "tok-founder", b.id, aid)}))
		return err
	}
	if err := decide("a", "USD"); err != nil {
		t.Fatalf("no caps configured, USD payment: %v", err)
	}
	big := int64(1_000_000_00)
	if err := e.store.SetSpendCap(ctx, b.uuid, store.SpendCap{Currency: "USD", PerTransaction: &big, UpdatedBy: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := decide("b", "USD"); err != nil {
		t.Fatalf("USD has a cap, USD payment under it: %v", err)
	}
	if err := decide("c", "EUR"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("EUR has no cap while USD does: want blocked, got %v", err)
	}
}

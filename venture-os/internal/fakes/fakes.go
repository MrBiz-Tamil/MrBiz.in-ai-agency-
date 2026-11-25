// Package fakes provides in-memory implementations of every port in internal/ports.
//
// They are not mocks that return canned values: each fake keeps real per-tenant state, honours the ports'
// idempotency and tenant-scoping contract, records every call, and can be told to fail, so the provisioner's
// retry / compensation logic and the MCP bus can be tested end to end without any external system.
package fakes

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/ports"
)

// Call is one recorded invocation.
type Call struct {
	System string
	Op     string
	Tenant uuid.UUID
}

// Hub is shared by all fakes of one test: one call log, one set of fault rules, one clock for latency.
type Hub struct {
	mu     sync.Mutex
	calls  []Call
	faults map[string]*fault
	// Latency is slept (honouring ctx) before each call to an external system (not the store); use it to test
	// timeouts, cancellation and parallelism.
	Latency time.Duration
}

type fault struct {
	err       error
	remaining int // -1 = forever
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{faults: map[string]*fault{}} }

// Fail makes the next n calls of system.op return err. n < 0 fails every call until ClearFaults.
// Wrap ports.ErrTransient to simulate a retryable failure.
func (h *Hub) Fail(system, op string, err error, n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.faults[system+"."+op] = &fault{err: err, remaining: n}
}

// ClearFaults removes every injected failure.
func (h *Hub) ClearFaults() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.faults = map[string]*fault{}
}

// Calls returns a copy of the call log in order.
func (h *Hub) Calls() []Call {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Call(nil), h.calls...)
}

// CallCount counts logged calls of system.op for tenant (uuid.Nil = any tenant).
func (h *Hub) CallCount(system, op string, tenant uuid.UUID) int {
	n := 0
	for _, c := range h.Calls() {
		if c.System == system && c.Op == op && (tenant == uuid.Nil || c.Tenant == tenant) {
			n++
		}
	}
	return n
}

// Ops lists "system.op" for every call, in order. Handy for asserting provisioning and rollback order.
func (h *Hub) Ops() []string {
	calls := h.Calls()
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.System + "." + c.Op
	}
	return out
}

// enter records the call, applies latency and returns an injected fault if one is armed.
func (h *Hub) enter(ctx context.Context, system, op string, tenant uuid.UUID) error {
	h.mu.Lock()
	h.calls = append(h.calls, Call{System: system, Op: op, Tenant: tenant})
	latency := h.Latency
	if system == "store" {
		latency = 0 // Latency models slow remote systems; the store is the control plane's own database
	}
	var injected error
	if f, ok := h.faults[system+"."+op]; ok && f.remaining != 0 {
		injected = f.err
		if f.remaining > 0 {
			f.remaining--
		}
	}
	h.mu.Unlock()

	if latency > 0 {
		select {
		case <-time.After(latency):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if tenant == uuid.Nil {
		return fmt.Errorf("%s.%s: tenant id is required", system, op)
	}
	return injected
}

func ref(prefix string, t ports.TenantInfo) ports.Ref {
	return ports.Ref(prefix + "-" + t.ID.String()[:8])
}

// ErrNotFound is returned by fakes when a dependent object does not exist (e.g. applying pipelines to an
// unknown workspace) - the same mistake a real adapter would hit.
var ErrNotFound = errors.New("not found")

// ───────────────────────────── identity ─────────────────────────────

type Identity struct {
	hub *Hub
	mu  sync.Mutex
	// Orgs maps tenant -> organization ref; Members maps tenant -> founder subject.
	orgs    map[uuid.UUID]ports.Ref
	founder map[uuid.UUID]string
}

func NewIdentity(h *Hub) *Identity {
	return &Identity{hub: h, orgs: map[uuid.UUID]ports.Ref{}, founder: map[uuid.UUID]string{}}
}

func (f *Identity) EnsureOrganization(ctx context.Context, t ports.TenantInfo, founderSub string) (ports.Ref, error) {
	if err := f.hub.enter(ctx, "identity", "EnsureOrganization", t.ID); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.orgs[t.ID]; ok {
		return r, nil
	}
	f.orgs[t.ID] = ref("kc-org", t)
	f.founder[t.ID] = founderSub
	return f.orgs[t.ID], nil
}

func (f *Identity) RemoveOrganization(ctx context.Context, t ports.TenantInfo, _ ports.Ref) error {
	if err := f.hub.enter(ctx, "identity", "RemoveOrganization", t.ID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.orgs, t.ID)
	delete(f.founder, t.ID)
	return nil
}

func (f *Identity) HasOrganization(tenant uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.orgs[tenant]
	return ok
}

// ───────────────────────────── ERP ─────────────────────────────

// ERPCompany is what the fake ERP stores per tenant.
type ERPCompany struct {
	Ref      ports.Ref
	Profile  domain.BusinessProfile
	Accounts []domain.Account
	TaxRates []domain.TaxRate
}

type ERP struct {
	hub *Hub
	mu  sync.Mutex
	co  map[uuid.UUID]ERPCompany
}

func NewERP(h *Hub) *ERP { return &ERP{hub: h, co: map[uuid.UUID]ERPCompany{}} }

func (f *ERP) EnsureCompany(ctx context.Context, t ports.TenantInfo, p domain.BusinessProfile, a domain.Accounting) (ports.Ref, error) {
	if err := f.hub.enter(ctx, "erp", "EnsureCompany", t.ID); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.co[t.ID]; ok {
		return c.Ref, nil
	}
	c := ERPCompany{Ref: ref("erp", t), Profile: p,
		Accounts: append([]domain.Account(nil), a.ChartOfAccounts...),
		TaxRates: append([]domain.TaxRate(nil), a.Tax.Rates...)}
	f.co[t.ID] = c
	return c.Ref, nil
}

func (f *ERP) RemoveCompany(ctx context.Context, t ports.TenantInfo, _ ports.Ref) error {
	if err := f.hub.enter(ctx, "erp", "RemoveCompany", t.ID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.co, t.ID)
	return nil
}

// Company returns the tenant's company, if provisioned.
func (f *ERP) Company(tenant uuid.UUID) (ERPCompany, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.co[tenant]
	return c, ok
}

// ───────────────────────────── CRM ─────────────────────────────

type CRM struct {
	hub       *Hub
	mu        sync.Mutex
	workspace map[uuid.UUID]ports.Ref
	pipelines map[uuid.UUID][]domain.Pipeline
}

func NewCRM(h *Hub) *CRM {
	return &CRM{hub: h, workspace: map[uuid.UUID]ports.Ref{}, pipelines: map[uuid.UUID][]domain.Pipeline{}}
}

func (f *CRM) EnsureWorkspace(ctx context.Context, t ports.TenantInfo) (ports.Ref, error) {
	if err := f.hub.enter(ctx, "crm", "EnsureWorkspace", t.ID); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.workspace[t.ID]; ok {
		return r, nil
	}
	f.workspace[t.ID] = ref("twenty-ws", t)
	return f.workspace[t.ID], nil
}

func (f *CRM) ApplyPipelines(ctx context.Context, t ports.TenantInfo, ws ports.Ref, p []domain.Pipeline) error {
	if err := f.hub.enter(ctx, "crm", "ApplyPipelines", t.ID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got, ok := f.workspace[t.ID]; !ok || got != ws {
		return fmt.Errorf("crm workspace %q for tenant %s: %w", ws, t.ID, ErrNotFound)
	}
	f.pipelines[t.ID] = append([]domain.Pipeline(nil), p...) // replace-all keeps re-application idempotent
	return nil
}

func (f *CRM) RemoveWorkspace(ctx context.Context, t ports.TenantInfo, _ ports.Ref) error {
	if err := f.hub.enter(ctx, "crm", "RemoveWorkspace", t.ID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.workspace, t.ID)
	delete(f.pipelines, t.ID)
	return nil
}

func (f *CRM) Pipelines(tenant uuid.UUID) []domain.Pipeline {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.Pipeline(nil), f.pipelines[tenant]...)
}

func (f *CRM) HasWorkspace(tenant uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.workspace[tenant]
	return ok
}

// ───────────────────────────── Support ─────────────────────────────

type Support struct {
	hub      *Hub
	mu       sync.Mutex
	inboxes  map[uuid.UUID]ports.Ref
	channels map[uuid.UUID][]domain.Channel
	docs     map[uuid.UUID][]domain.KBDoc
}

func NewSupport(h *Hub) *Support {
	return &Support{hub: h, inboxes: map[uuid.UUID]ports.Ref{}, channels: map[uuid.UUID][]domain.Channel{}, docs: map[uuid.UUID][]domain.KBDoc{}}
}

func (f *Support) EnsureInboxes(ctx context.Context, t ports.TenantInfo, ch []domain.Channel) (ports.Ref, error) {
	if err := f.hub.enter(ctx, "support", "EnsureInboxes", t.ID); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.inboxes[t.ID]; ok {
		return r, nil
	}
	f.inboxes[t.ID] = ref("ld-inbox", t)
	f.channels[t.ID] = append([]domain.Channel(nil), ch...)
	return f.inboxes[t.ID], nil
}

func (f *Support) SeedKnowledgeBase(ctx context.Context, t ports.TenantInfo, inbox ports.Ref, docs []domain.KBDoc) error {
	if err := f.hub.enter(ctx, "support", "SeedKnowledgeBase", t.ID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got, ok := f.inboxes[t.ID]; !ok || got != inbox {
		return fmt.Errorf("support inbox %q for tenant %s: %w", inbox, t.ID, ErrNotFound)
	}
	f.docs[t.ID] = append([]domain.KBDoc(nil), docs...)
	return nil
}

func (f *Support) RemoveInboxes(ctx context.Context, t ports.TenantInfo, _ ports.Ref) error {
	if err := f.hub.enter(ctx, "support", "RemoveInboxes", t.ID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.inboxes, t.ID)
	delete(f.channels, t.ID)
	delete(f.docs, t.ID)
	return nil
}

func (f *Support) KBDocs(tenant uuid.UUID) []domain.KBDoc {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.KBDoc(nil), f.docs[tenant]...)
}

func (f *Support) HasInboxes(tenant uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.inboxes[tenant]
	return ok
}

// ───────────────────────────── Hermes ─────────────────────────────

type Runtime struct {
	hub     *Hub
	mu      sync.Mutex
	profile map[uuid.UUID]ports.Ref
	token   map[uuid.UUID]string
	skills  map[uuid.UUID]map[string]ports.Skill
}

func NewRuntime(h *Hub) *Runtime {
	return &Runtime{hub: h, profile: map[uuid.UUID]ports.Ref{}, token: map[uuid.UUID]string{}, skills: map[uuid.UUID]map[string]ports.Skill{}}
}

func (f *Runtime) EnsureProfile(ctx context.Context, t ports.TenantInfo, mcpToken string) (ports.Ref, error) {
	if err := f.hub.enter(ctx, "runtime", "EnsureProfile", t.ID); err != nil {
		return "", err
	}
	if mcpToken == "" {
		return "", errors.New("runtime.EnsureProfile: an MCP token is required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.profile[t.ID]; ok {
		f.token[t.ID] = mcpToken // token rotation is allowed; the profile identity is stable
		return r, nil
	}
	f.profile[t.ID] = ref("hermes", t)
	f.token[t.ID] = mcpToken
	f.skills[t.ID] = map[string]ports.Skill{}
	return f.profile[t.ID], nil
}

func (f *Runtime) InstallSkills(ctx context.Context, t ports.TenantInfo, profile ports.Ref, skills []ports.Skill) error {
	if err := f.hub.enter(ctx, "runtime", "InstallSkills", t.ID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got, ok := f.profile[t.ID]; !ok || got != profile {
		return fmt.Errorf("runtime profile %q for tenant %s: %w", profile, t.ID, ErrNotFound)
	}
	for _, s := range skills {
		f.skills[t.ID][s.AgentKey] = s
	}
	return nil
}

func (f *Runtime) RemoveProfile(ctx context.Context, t ports.TenantInfo, _ ports.Ref) error {
	if err := f.hub.enter(ctx, "runtime", "RemoveProfile", t.ID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.profile, t.ID)
	delete(f.token, t.ID)
	delete(f.skills, t.ID)
	return nil
}

// Skills returns the installed skills for tenant, sorted by agent key.
func (f *Runtime) Skills(tenant uuid.UUID) []ports.Skill {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ports.Skill, 0, len(f.skills[tenant]))
	for _, s := range f.skills[tenant] {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentKey < out[j].AgentKey })
	return out
}

func (f *Runtime) HasProfile(tenant uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.profile[tenant]
	return ok
}

// TokenFor exposes the token a profile was bound to, so tests can assert tenants never share one.
func (f *Runtime) TokenFor(tenant uuid.UUID) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.token[tenant]
}

// ───────────────────────────── LLM ─────────────────────────────

// LLM replays scripted replies in order and records every request. With no script left it fails loudly rather
// than inventing an answer, so a test never silently depends on model behaviour.
type LLM struct {
	hub      *Hub
	mu       sync.Mutex
	replies  []ports.LLMResponse
	requests []ports.LLMRequest
}

func NewLLM(h *Hub) *LLM { return &LLM{hub: h} }

// Script appends replies to be returned by successive Complete calls.
func (f *LLM) Script(contents ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range contents {
		f.replies = append(f.replies, ports.LLMResponse{Content: c, InputTokens: len(c) / 4, OutputTokens: len(c) / 4})
	}
}

func (f *LLM) Complete(ctx context.Context, req ports.LLMRequest) (ports.LLMResponse, error) {
	if err := f.hub.enter(ctx, "llm", "Complete", uuid.MustParse("00000000-0000-0000-0000-000000000001")); err != nil {
		return ports.LLMResponse{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if len(f.replies) == 0 {
		return ports.LLMResponse{}, errors.New("fakes.LLM: no scripted reply left")
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	return r, nil
}

func (f *LLM) Requests() []ports.LLMRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ports.LLMRequest(nil), f.requests...)
}

// Compile-time proof that every fake satisfies its port.
var (
	_ ports.IdentityProvider = (*Identity)(nil)
	_ ports.ERP              = (*ERP)(nil)
	_ ports.CRM              = (*CRM)(nil)
	_ ports.Support          = (*Support)(nil)
	_ ports.AgentRuntime     = (*Runtime)(nil)
	_ ports.LLM              = (*LLM)(nil)
)

// ExternalCalls counts logged calls to every system except the control plane's own store.
func (h *Hub) ExternalCalls() int {
	n := 0
	for _, c := range h.Calls() {
		if c.System != "store" {
			n++
		}
	}
	return n
}

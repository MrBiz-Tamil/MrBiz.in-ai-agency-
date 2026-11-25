// Package controlplane implements the ConnectRPC services of the Venture OS control plane: tenants, Genesis and
// the Provisioner. Every handler authenticates the caller (internal/auth), then authorises them against the
// tenant_members table before touching tenant data; row-level security in Postgres is a second, independent
// line of defence.
package controlplane

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/gen/venture/agent/v1/agentv1connect"
	"github.com/mrbiz/venture-os/gen/venture/approval/v1/approvalv1connect"
	"github.com/mrbiz/venture-os/gen/venture/connection/v1/connectionv1connect"
	"github.com/mrbiz/venture-os/gen/venture/controls/v1/controlsv1connect"
	"github.com/mrbiz/venture-os/gen/venture/crm/v1/crmv1connect"
	genesisv1 "github.com/mrbiz/venture-os/gen/venture/genesis/v1"
	"github.com/mrbiz/venture-os/gen/venture/genesis/v1/genesisv1connect"
	"github.com/mrbiz/venture-os/gen/venture/portfolio/v1/portfoliov1connect"
	provisionerv1 "github.com/mrbiz/venture-os/gen/venture/provisioner/v1"
	"github.com/mrbiz/venture-os/gen/venture/provisioner/v1/provisionerv1connect"
	tenantv1 "github.com/mrbiz/venture-os/gen/venture/tenant/v1"
	"github.com/mrbiz/venture-os/gen/venture/tenant/v1/tenantv1connect"
	"github.com/mrbiz/venture-os/internal/auth"
	"github.com/mrbiz/venture-os/internal/collabsvc"
	"github.com/mrbiz/venture-os/internal/crm"
	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/financeengine"
	"github.com/mrbiz/venture-os/internal/genesis"
	"github.com/mrbiz/venture-os/internal/mcptoken"
	"github.com/mrbiz/venture-os/internal/ports"
	"github.com/mrbiz/venture-os/internal/provisioner"
	"github.com/mrbiz/venture-os/internal/store"
	"github.com/mrbiz/venture-os/internal/vault"
)

// Repo is the persistence the services need (implemented by *store.PG).
type Repo interface {
	CreateTenant(ctx context.Context, founderSub, key string) (uuid.UUID, bool, error)
	Memberships(ctx context.Context, userSub string) ([]store.Membership, error)
	Tenant(ctx context.Context, id uuid.UUID) (store.Tenant, error)
	StartGenesisSession(ctx context.Context, tenant uuid.UUID, createdBy, key string) (uuid.UUID, error)
	GenesisState(ctx context.Context, tenant, session uuid.UUID) ([]store.Message, *domain.Blueprint, string, error)
	TurnReply(ctx context.Context, tenant uuid.UUID, turnKey string) (string, bool, error)
	RecordTurn(ctx context.Context, tenant, session uuid.UUID, turnKey, founderMsg, assistantMsg string, draft *domain.Blueprint) (bool, error)
	SaveBlueprint(ctx context.Context, tenant uuid.UUID, session *uuid.UUID, bp domain.Blueprint, createdBy, key string) (store.StoredBlueprint, error)
	Blueprint(ctx context.Context, tenant, id uuid.UUID) (store.StoredBlueprint, error)
	ApproveBlueprint(ctx context.Context, tenant, id uuid.UUID, approvedBy string) (store.StoredBlueprint, error)
	LatestRun(ctx context.Context, tenant uuid.UUID) (store.ProvisionRun, error)
	AuditUser(ctx context.Context, tenant uuid.UUID, sub, action, entityType, entityID string, data map[string]any) error
	AuditActor(ctx context.Context, tenant uuid.UUID, actorType, actorID, action, entityType, entityID string, data map[string]any) error

	// Finance and governance.
	ApprovedBlueprint(ctx context.Context, tenant uuid.UUID) (store.StoredBlueprint, error)
	PricingRules(ctx context.Context, tenant uuid.UUID) ([]financeengine.Rule, error)
	AccountBalance(ctx context.Context, tenant uuid.UUID, code string) (int64, error)
	CashflowItems(ctx context.Context, tenant uuid.UUID, until time.Time) ([]financeengine.Item, error)
	AddCashflowItem(ctx context.Context, tenant uuid.UUID, it store.CashflowItem) (bool, error)
	PostJournal(ctx context.Context, tenant uuid.UUID, j store.Journal) (uuid.UUID, bool, error)
	CreateApproval(ctx context.Context, tenant uuid.UUID, n store.NewApproval) (store.Approval, bool, error)
	Approval(ctx context.Context, tenant, id uuid.UUID) (store.Approval, error)
	ListApprovals(ctx context.Context, tenants []uuid.UUID, status string, limit int) ([]store.Approval, error)
	PendingCounts(ctx context.Context, tenants []uuid.UUID) (map[uuid.UUID]store.PendingCount, error)
	DecideApproval(ctx context.Context, tenant, id uuid.UUID, approve bool, by, note string, wantHash [32]byte) (store.Approval, error)
	ClaimExecution(ctx context.Context, tenant, id uuid.UUID) (bool, error)
	FinishExecution(ctx context.Context, tenant, id uuid.UUID, ok bool, result json.RawMessage) error

	// Controls and connections.
	SpendCaps(ctx context.Context, tenant uuid.UUID) ([]store.SpendCap, error)
	SetSpendCap(ctx context.Context, tenant uuid.UUID, c store.SpendCap) error
	VerifyAudit(ctx context.Context, tenant uuid.UUID) (store.AuditVerdict, error)
	SaveConnection(ctx context.Context, tenant uuid.UUID, c store.Connection, sealed vault.Sealed) error
	Connection(ctx context.Context, tenant uuid.UUID, provider string) (store.Connection, vault.Sealed, error)
	Connections(ctx context.Context, tenant uuid.UUID) ([]store.Connection, error)
	DeleteConnection(ctx context.Context, tenant uuid.UUID, provider string) error
	BeginIdem(ctx context.Context, tenant uuid.UUID, key string, requestHash [32]byte) (store.IdemState, json.RawMessage, error)
	CompleteIdem(ctx context.Context, tenant uuid.UUID, key string, response json.RawMessage) error
	AbandonIdem(ctx context.Context, tenant uuid.UUID, key string) error
}

// Interviewer is the Genesis engine.
type Interviewer interface {
	Step(ctx context.Context, t genesis.Turn) (genesis.Reply, error)
}

// Provisioner is the saga runner.
type Provisioner interface {
	Provision(ctx context.Context, req provisioner.Request) (provisioner.Result, error)
}

// Server holds the dependencies of all three services.
type Server struct {
	Repo        Repo
	Interviewer Interviewer
	Provisioner Provisioner
	Log         *slog.Logger
	// AgentSigner verifies MCP bearer tokens. When nil, AgentService is not mounted.
	AgentSigner *mcptoken.Signer
	// Collab connects and serves meetings and notes providers (Google Calendar/Meet, Zoom, Notion). Nil when no secrets
	// key is configured: integrations are then unavailable rather than insecure.
	Collab *collabsvc.Registry
	// CRM serves the crm_ops tools. When nil the business has no CRM connected and those tools fail cleanly.
	CRM crm.Provider
	// Now is the clock (defaults to time.Now); tests inject a fixed one.
	Now func() time.Time
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

const maxKeyLen = 200

// maxRequestBytes caps every request message. Connect parses the message before the auth interceptor runs, so without a
// cap an unauthenticated caller could make the server buffer and parse an arbitrarily large body. The largest legitimate
// request is a few tens of KiB (a 64 KiB tool-argument or secret field), so 1 MiB leaves ample room.
const maxRequestBytes = 1 << 20

// Handler returns an http.Handler serving every service plus /healthz, with authentication applied.
func (s *Server) Handler(v auth.Verifier, opts ...connect.HandlerOption) http.Handler {
	// The size cap goes first so a caller-supplied option can still override it.
	size := connect.WithReadMaxBytes(maxRequestBytes)
	opts = append([]connect.HandlerOption{size}, opts...)
	opts = append(opts, connect.WithInterceptors(auth.Interceptor(v)))
	mux := http.NewServeMux()
	mux.Handle(tenantv1connect.NewTenantServiceHandler(&tenantSvc{s}, opts...))
	mux.Handle(genesisv1connect.NewGenesisServiceHandler(&genesisSvc{s}, opts...))
	mux.Handle(provisionerv1connect.NewProvisionerServiceHandler(&provisionerSvc{s}, opts...))
	mux.Handle(approvalv1connect.NewApprovalServiceHandler(&approvalSvc{s}, opts...))
	mux.Handle(crmv1connect.NewCrmServiceHandler(&crmSvc{s}, opts...))
	mux.Handle(connectionv1connect.NewConnectionServiceHandler(&connectionSvc{s}, opts...))
	mux.Handle(controlsv1connect.NewControlsServiceHandler(&controlsSvc{s}, opts...))
	mux.Handle(portfoliov1connect.NewPortfolioServiceHandler(&portfolioSvc{s}, opts...))
	if s.AgentSigner != nil {
		// Agents authenticate with MCP tokens, never with a user's OIDC session.
		mux.Handle(agentv1connect.NewAgentServiceHandler(&agentSvc{s}, size, connect.WithInterceptors(mcpInterceptor(s.AgentSigner))))
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	return mux
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// ───────────────────────────── authorisation ─────────────────────────────

type role int

const (
	roleViewer role = iota + 1
	roleApprover
	roleFounder
)

func parseRole(s string) role {
	switch s {
	case "founder":
		return roleFounder
	case "approver":
		return roleApprover
	case "viewer":
		return roleViewer
	}
	return 0
}

// authorize resolves the caller and checks they hold at least min in the tenant. A caller with no membership gets
// NotFound, indistinguishable from a tenant that does not exist, so tenant ids cannot be probed.
func (s *Server) authorize(ctx context.Context, tenantID string, min role) (auth.Principal, uuid.UUID, error) {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return auth.Principal{}, uuid.Nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}
	tenant, err := uuid.Parse(tenantID)
	if err != nil || tenant == uuid.Nil {
		return p, uuid.Nil, connect.NewError(connect.CodeInvalidArgument, errors.New("tenant_id must be a uuid"))
	}
	ms, err := s.Repo.Memberships(ctx, p.Sub)
	if err != nil {
		return p, tenant, s.internal("memberships", err)
	}
	for _, m := range ms {
		if m.Tenant.ID == tenant {
			if parseRole(m.Role) < min {
				return p, tenant, connect.NewError(connect.CodePermissionDenied, errors.New("your role in this business does not allow that"))
			}
			return p, tenant, nil
		}
	}
	return p, tenant, connect.NewError(connect.CodeNotFound, errors.New("business not found"))
}

func (s *Server) internal(what string, err error) error {
	s.log().Error("request failed", "what", what, "err", err)
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

func parseID(field, v string) (uuid.UUID, error) {
	id, err := uuid.Parse(v)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s must be a uuid", field))
	}
	return id, nil
}

func checkKey(field, v string) error {
	if strings.TrimSpace(v) == "" || len(v) > maxKeyLen {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s is required (max %d chars)", field, maxKeyLen))
	}
	return nil
}

func hexHash(h [32]byte) string { return hex.EncodeToString(h[:]) }

// ───────────────────────────── TenantService ─────────────────────────────

type tenantSvc struct{ *Server }

func (s *tenantSvc) CreateBusiness(ctx context.Context, req *connect.Request[tenantv1.CreateBusinessRequest]) (*connect.Response[tenantv1.CreateBusinessResponse], error) {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}
	if err := checkKey("idempotency_key", req.Msg.IdempotencyKey); err != nil {
		return nil, err
	}
	id, created, err := s.Repo.CreateTenant(ctx, p.Sub, req.Msg.IdempotencyKey)
	if err != nil {
		return nil, s.internal("create tenant", err)
	}
	if created {
		_ = s.Repo.AuditUser(ctx, id, p.Sub, "tenant.create", "tenant", id.String(), nil)
	}
	return connect.NewResponse(&tenantv1.CreateBusinessResponse{TenantId: id.String(), Created: created}), nil
}

func (s *tenantSvc) ListBusinesses(ctx context.Context, _ *connect.Request[tenantv1.ListBusinessesRequest]) (*connect.Response[tenantv1.ListBusinessesResponse], error) {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}
	ms, err := s.Repo.Memberships(ctx, p.Sub)
	if err != nil {
		return nil, s.internal("list businesses", err)
	}
	out := &tenantv1.ListBusinessesResponse{}
	for _, m := range ms {
		out.Businesses = append(out.Businesses, &tenantv1.Business{
			TenantId: m.Tenant.ID.String(), Slug: m.Tenant.Slug, Name: m.Tenant.Name, Status: m.Tenant.Status,
			Role: m.Role, Industry: m.Tenant.Industry, Country: m.Tenant.Country, BaseCurrency: m.Tenant.BaseCurrency,
		})
	}
	return connect.NewResponse(out), nil
}

// ───────────────────────────── GenesisService ─────────────────────────────

type genesisSvc struct{ *Server }

func (s *genesisSvc) StartSession(ctx context.Context, req *connect.Request[genesisv1.StartSessionRequest]) (*connect.Response[genesisv1.StartSessionResponse], error) {
	p, tenant, err := s.authorize(ctx, req.Msg.TenantId, roleFounder)
	if err != nil {
		return nil, err
	}
	if err := checkKey("idempotency_key", req.Msg.IdempotencyKey); err != nil {
		return nil, err
	}
	id, err := s.Repo.StartGenesisSession(ctx, tenant, p.Sub, req.Msg.IdempotencyKey)
	if err != nil {
		return nil, s.internal("start session", err)
	}
	return connect.NewResponse(&genesisv1.StartSessionResponse{SessionId: id.String()}), nil
}

func (s *genesisSvc) Converse(ctx context.Context, req *connect.Request[genesisv1.ConverseRequest]) (*connect.Response[genesisv1.ConverseResponse], error) {
	_, tenant, err := s.authorize(ctx, req.Msg.TenantId, roleFounder)
	if err != nil {
		return nil, err
	}
	session, err := parseID("session_id", req.Msg.SessionId)
	if err != nil {
		return nil, err
	}
	if err := checkKey("turn_key", req.Msg.TurnKey); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Msg.Message) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("message is required"))
	}
	if len(req.Msg.Message) > genesis.MaxFounderMessageBytes {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("message exceeds %d bytes", genesis.MaxFounderMessageBytes))
	}

	history, draft, status, err := s.Repo.GenesisState(ctx, tenant, session)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("session not found"))
	}
	if err != nil {
		return nil, s.internal("genesis state", err)
	}
	if status != "ACTIVE" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("session is %s", strings.ToLower(status)))
	}

	// A retried turn is answered from storage: no second model call, no duplicate transcript.
	if stored, found, err := s.Repo.TurnReply(ctx, tenant, req.Msg.TurnKey); err != nil {
		return nil, s.internal("turn reply", err)
	} else if found {
		return connect.NewResponse(&genesisv1.ConverseResponse{AssistantMessage: stored, DraftBlueprintJson: marshalDraft(draft), Replayed: true}), nil
	}

	turn := genesis.Turn{Draft: draft, FounderMessage: req.Msg.Message}
	for _, m := range history {
		turn.History = append(turn.History, genesis.Message{Role: m.Role, Content: m.Content})
	}
	reply, err := s.Interviewer.Step(ctx, turn)
	switch {
	case errors.Is(err, genesis.ErrMessageTooLarge):
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, ports.ErrTransient):
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("the assistant is busy, please retry"))
	case err != nil:
		return nil, s.internal("genesis step", err)
	}

	var newDraft *domain.Blueprint
	if reply.DraftChanged {
		newDraft = reply.Draft
	}
	if _, err := s.Repo.RecordTurn(ctx, tenant, session, req.Msg.TurnKey, req.Msg.Message, reply.Message, newDraft); err != nil {
		return nil, s.internal("record turn", err)
	}
	return connect.NewResponse(&genesisv1.ConverseResponse{
		AssistantMessage: reply.Message, DraftBlueprintJson: marshalDraft(reply.Draft),
		DraftChanged: reply.DraftChanged, Ready: reply.Ready, Problems: reply.Problems,
	}), nil
}

func marshalDraft(bp *domain.Blueprint) []byte {
	if bp == nil {
		return nil
	}
	raw, err := json.Marshal(bp)
	if err != nil {
		return nil
	}
	return raw
}

func (s *genesisSvc) GetSession(ctx context.Context, req *connect.Request[genesisv1.GetSessionRequest]) (*connect.Response[genesisv1.GetSessionResponse], error) {
	_, tenant, err := s.authorize(ctx, req.Msg.TenantId, roleViewer)
	if err != nil {
		return nil, err
	}
	session, err := parseID("session_id", req.Msg.SessionId)
	if err != nil {
		return nil, err
	}
	msgs, draft, status, err := s.Repo.GenesisState(ctx, tenant, session)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("session not found"))
	}
	if err != nil {
		return nil, s.internal("get session", err)
	}
	out := &genesisv1.GetSessionResponse{Status: status, DraftBlueprintJson: marshalDraft(draft)}
	for _, m := range msgs {
		out.Turns = append(out.Turns, &genesisv1.Turn{Role: m.Role, Content: m.Content})
	}
	return connect.NewResponse(out), nil
}

func (s *genesisSvc) SubmitBlueprint(ctx context.Context, req *connect.Request[genesisv1.SubmitBlueprintRequest]) (*connect.Response[genesisv1.SubmitBlueprintResponse], error) {
	p, tenant, err := s.authorize(ctx, req.Msg.TenantId, roleFounder)
	if err != nil {
		return nil, err
	}
	session, err := parseID("session_id", req.Msg.SessionId)
	if err != nil {
		return nil, err
	}
	if err := checkKey("idempotency_key", req.Msg.IdempotencyKey); err != nil {
		return nil, err
	}
	_, draft, _, err := s.Repo.GenesisState(ctx, tenant, session)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("session not found"))
	}
	if err != nil {
		return nil, s.internal("submit: state", err)
	}
	if draft == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("there is no valid draft to submit yet"))
	}
	sb, err := s.Repo.SaveBlueprint(ctx, tenant, &session, *draft, p.Sub, req.Msg.IdempotencyKey)
	if domain.IsValidationError(err) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if err != nil {
		return nil, s.internal("submit: save", err)
	}
	_ = s.Repo.AuditUser(ctx, tenant, p.Sub, "blueprint.submit", "blueprint", sb.ID.String(), map[string]any{"version": sb.Version})
	return connect.NewResponse(&genesisv1.SubmitBlueprintResponse{
		BlueprintId: sb.ID.String(), Version: int32(sb.Version), ContentHash: hexHash(sb.Hash), Status: sb.Status,
	}), nil
}

func (s *genesisSvc) GetBlueprint(ctx context.Context, req *connect.Request[genesisv1.GetBlueprintRequest]) (*connect.Response[genesisv1.GetBlueprintResponse], error) {
	_, tenant, err := s.authorize(ctx, req.Msg.TenantId, roleViewer)
	if err != nil {
		return nil, err
	}
	id, err := parseID("blueprint_id", req.Msg.BlueprintId)
	if err != nil {
		return nil, err
	}
	sb, err := s.Repo.Blueprint(ctx, tenant, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("blueprint not found"))
	}
	if err != nil {
		return nil, s.internal("get blueprint", err)
	}
	raw, _ := sb.Blueprint.CanonicalJSON()
	return connect.NewResponse(&genesisv1.GetBlueprintResponse{
		BlueprintId: sb.ID.String(), Version: int32(sb.Version), Status: sb.Status,
		BlueprintJson: raw, ContentHash: hexHash(sb.Hash), ApprovedBy: sb.ApprovedBy,
	}), nil
}

func (s *genesisSvc) ApproveBlueprint(ctx context.Context, req *connect.Request[genesisv1.ApproveBlueprintRequest]) (*connect.Response[genesisv1.ApproveBlueprintResponse], error) {
	p, tenant, err := s.authorize(ctx, req.Msg.TenantId, roleFounder)
	if err != nil {
		return nil, err
	}
	id, err := parseID("blueprint_id", req.Msg.BlueprintId)
	if err != nil {
		return nil, err
	}
	sb, err := s.Repo.Blueprint(ctx, tenant, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("blueprint not found"))
	}
	if err != nil {
		return nil, s.internal("approve: load", err)
	}
	// The founder approves what they were shown: refuse if the stored content differs from that hash.
	if !strings.EqualFold(req.Msg.ContentHash, hexHash(sb.Hash)) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("content_hash does not match this blueprint; reload and review it"))
	}
	approved, err := s.Repo.ApproveBlueprint(ctx, tenant, id, p.Sub)
	switch {
	case errors.Is(err, store.ErrSlugTaken):
		return nil, connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, store.ErrTenantLive), errors.Is(err, store.ErrConflict):
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case err != nil:
		return nil, s.internal("approve", err)
	}
	_ = s.Repo.AuditUser(ctx, tenant, p.Sub, "blueprint.approve", "blueprint", id.String(), map[string]any{"content_hash": hexHash(approved.Hash)})
	return connect.NewResponse(&genesisv1.ApproveBlueprintResponse{BlueprintId: approved.ID.String(), Status: approved.Status}), nil
}

// ───────────────────────────── ProvisionerService ─────────────────────────────

type provisionerSvc struct{ *Server }

func steps(in []provisioner.StepRecord) []*provisionerv1.Step {
	out := make([]*provisionerv1.Step, len(in))
	for i, r := range in {
		out[i] = &provisionerv1.Step{Key: r.Key, Status: string(r.Status), Attempts: int32(r.Attempts), Error: r.Error}
	}
	return out
}

func (s *provisionerSvc) Provision(ctx context.Context, req *connect.Request[provisionerv1.ProvisionRequest]) (*connect.Response[provisionerv1.ProvisionResponse], error) {
	p, tenant, err := s.authorize(ctx, req.Msg.TenantId, roleFounder)
	if err != nil {
		return nil, err
	}
	bpID, err := parseID("blueprint_id", req.Msg.BlueprintId)
	if err != nil {
		return nil, err
	}
	if err := checkKey("idempotency_key", req.Msg.IdempotencyKey); err != nil {
		return nil, err
	}
	sb, err := s.Repo.Blueprint(ctx, tenant, bpID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("blueprint not found"))
	}
	if err != nil {
		return nil, s.internal("provision: blueprint", err)
	}
	if sb.Status != "APPROVED" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("only an approved blueprint can be provisioned"))
	}
	t, err := s.Repo.Tenant(ctx, tenant)
	if err != nil {
		return nil, s.internal("provision: tenant", err)
	}

	res, err := s.Provisioner.Provision(ctx, provisioner.Request{
		Tenant: t.Info(), FounderSub: p.Sub, BlueprintID: bpID, Blueprint: sb.Blueprint, IdempotencyKey: req.Msg.IdempotencyKey,
	})
	var perr *provisioner.Error
	switch {
	case err == nil:
	case errors.Is(err, provisioner.ErrInvalidRequest):
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, provisioner.ErrInProgress), errors.Is(err, store.ErrTenantBusy):
		return nil, connect.NewError(connect.CodeAborted, errors.New("provisioning is already in progress; retry shortly"))
	case errors.Is(err, store.ErrNotApproved):
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.As(err, &perr):
		s.log().Warn("provisioning failed", "tenant", tenant, "run", perr.RunID, "step", perr.Step, "status", perr.Status, "err", perr.Cause)
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("provisioning failed at %q and was %s; see GetStatus for details", perr.Step, strings.ToLower(strings.ReplaceAll(string(perr.Status), "_", " "))))
	default:
		return nil, s.internal("provision", err)
	}
	return connect.NewResponse(&provisionerv1.ProvisionResponse{
		RunId: res.RunID.String(), Status: string(res.Status), Replayed: res.Replayed,
		Steps: steps(res.Steps), ElapsedMs: res.Elapsed.Milliseconds(),
	}), nil
}

func (s *provisionerSvc) GetStatus(ctx context.Context, req *connect.Request[provisionerv1.GetStatusRequest]) (*connect.Response[provisionerv1.GetStatusResponse], error) {
	_, tenant, err := s.authorize(ctx, req.Msg.TenantId, roleViewer)
	if err != nil {
		return nil, err
	}
	run, err := s.Repo.LatestRun(ctx, tenant)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no provisioning run yet"))
	}
	if err != nil {
		return nil, s.internal("get status", err)
	}
	return connect.NewResponse(&provisionerv1.GetStatusResponse{
		RunId: run.Run.ID.String(), Status: string(run.Run.Status), Error: run.Run.Error, Steps: steps(run.Steps),
	}), nil
}

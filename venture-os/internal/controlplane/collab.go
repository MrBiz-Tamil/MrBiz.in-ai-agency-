package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect"

	agentv1 "github.com/mrbiz/venture-os/gen/venture/agent/v1"
	connectionv1 "github.com/mrbiz/venture-os/gen/venture/connection/v1"
	controlsv1 "github.com/mrbiz/venture-os/gen/venture/controls/v1"
	"github.com/mrbiz/venture-os/internal/collab"
	"github.com/mrbiz/venture-os/internal/collabops"
	"github.com/mrbiz/venture-os/internal/collabsvc"
	"github.com/mrbiz/venture-os/internal/store"
)

func (s *Server) noIntegrations() error {
	return connect.NewError(connect.CodeFailedPrecondition, errors.New("integrations are not configured on this server (VENTURE_SECRETS_KEYS is not set)"))
}

// collabErr maps provider errors to codes whose messages are safe and useful to show an agent or a person.
func (s *Server) collabErr(what string, err error) error {
	switch {
	case errors.Is(err, collab.ErrInvalid):
		return connect.NewError(connect.CodeInvalidArgument, trimPrefix(err, "collab: invalid input: "))
	case errors.Is(err, collab.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, trimPrefix(err, "collab: not found: "))
	case errors.Is(err, collab.ErrNotConnected):
		return connect.NewError(connect.CodeFailedPrecondition, trimPrefix(err, "collab: provider not connected: "))
	case errors.Is(err, collab.ErrAuth):
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("the provider rejected this business's stored credentials; the founder needs to reconnect it"))
	case errors.Is(err, collab.ErrUnavailable):
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("the provider did not confirm the request, so it may or may not have happened. Retry with the SAME idempotency_key, never a new one, and check the provider if it keeps failing"))
	}
	return s.internal("collab "+what, err)
}

func trimPrefix(err error, p string) error { return errors.New(strings.TrimPrefix(err.Error(), p)) }

func truncate(v string, max int) string {
	if utf8.RuneCountInString(v) <= max {
		return v
	}
	return string([]rune(v)[:max-1]) + "…"
}

// ───────────────────────────── agents ─────────────────────────────

// CollabOperation runs one meetings_ops / notes_ops operation for the calling agent's tenant. What would email other
// people is queued as a MEETING approval instead. External writes that the provider cannot de-duplicate are made
// idempotent here with the platform's idempotency keys, and an attempt whose outcome is unknown is never silently repeated.
func (s *agentSvc) CollabOperation(ctx context.Context, req *connect.Request[agentv1.CollabOperationRequest]) (*connect.Response[agentv1.CollabOperationResponse], error) {
	claims, t, _, err := s.session(ctx)
	if err != nil {
		return nil, err
	}
	if s.Collab == nil {
		return nil, s.noIntegrations()
	}
	op, ok := collabops.Find(req.Msg.Operation)
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("unknown operation"))
	}
	if len(req.Msg.ArgsJson) > maxCRMArgsBytes {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("arguments are too large"))
	}
	svc, err := s.Collab.For(ctx, t.ID.String())
	if err != nil {
		return nil, s.collabErr("connections", err)
	}
	who := requester(claims)

	var key string
	var idemKey string
	if !op.ReadOnly {
		var k struct {
			Key string `json:"idempotency_key"`
		}
		_ = json.Unmarshal(req.Msg.ArgsJson, &k) // strict decoding happens in the operation; here we only need the key
		if err := checkKey("idempotency_key", k.Key); err != nil {
			return nil, err
		}
		key = k.Key
		idemKey = "collab:" + op.Name + ":" + key
		sum := sha256.Sum256(append([]byte(op.Name+"\x00"), req.Msg.ArgsJson...))
		state, stored, err := s.Repo.BeginIdem(ctx, t.ID, idemKey, sum)
		switch {
		case errors.Is(err, store.ErrIdempotencyMismatch):
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("that idempotency_key was already used for a different request; use a new key for a different request"))
		case err != nil:
			return nil, s.internal("collab idempotency", err)
		case state == store.IdemDone:
			return connect.NewResponse(&agentv1.CollabOperationResponse{ResultJson: replayed(stored)}), nil
		case state == store.IdemUnknown:
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("an earlier attempt with this idempotency_key started but its outcome was not recorded, so it may have reached the provider. Check the provider before repeating it, and use a NEW key only for a genuinely new request"))
		}
	}

	res, err := op.Run(ctx, svc, s.now(), who, req.Msg.ArgsJson)
	if err != nil {
		if idemKey != "" && !errors.Is(err, collab.ErrUnavailable) {
			_ = s.Repo.AbandonIdem(ctx, t.ID, idemKey) // rejected before anything happened: a retry may proceed
		}
		return nil, s.collabErr(op.Name, err)
	}

	if res.Approval != nil {
		a, created, err := s.Repo.CreateApproval(ctx, t.ID, store.NewApproval{
			Kind: "MEETING", Title: truncate(res.Approval.Title, 200), Summary: truncate(res.Approval.Summary, 2000),
			RequestedByType: "AGENT", RequestedBy: who, Risk: "MEDIUM", Payload: res.Approval.Payload,
			IdempotencyKey: "request:" + idemKey,
		})
		if err != nil {
			_ = s.Repo.AbandonIdem(ctx, t.ID, idemKey)
			return nil, s.approvalErr("agent: queue meeting approval", err)
		}
		s.auditAgent(ctx, claims, t.ID, "approval.request", a)
		out, _ := json.Marshal(map[string]any{
			"status": "PENDING_APPROVAL", "approval_id": a.ID.String(), "created": created,
			"message": "Queued for the founder's approval as " + a.ID.String() + ". NOTHING has been sent, created or cancelled yet. Do not proceed as if it were approved; check back with get_approval_status.",
		})
		_ = s.Repo.CompleteIdem(ctx, t.ID, idemKey, out)
		return connect.NewResponse(&agentv1.CollabOperationResponse{ResultJson: out}), nil
	}

	out, err := json.Marshal(res.Data)
	if err != nil {
		return nil, s.internal("collab result", err)
	}
	if !op.ReadOnly {
		_ = s.Repo.CompleteIdem(ctx, t.ID, idemKey, out)
		if err := s.Repo.AuditActor(ctx, t.ID, "AGENT", who, "collab."+op.Name, "collab", res.EntityID, map[string]any{}); err != nil {
			s.log().Error("audit write failed", "action", "collab."+op.Name, "err", err)
		}
	}
	return connect.NewResponse(&agentv1.CollabOperationResponse{ResultJson: out}), nil
}

// replayed marks a stored response as a replay so the agent knows nothing new happened.
func replayed(stored json.RawMessage) []byte {
	var m map[string]any
	if json.Unmarshal(stored, &m) != nil || m == nil {
		return stored
	}
	m["replayed"] = true
	out, _ := json.Marshal(m)
	return out
}

// executeMeeting performs an approved MEETING request. The approval id seeds the provider request id, so running it
// again after a crash returns the same meeting instead of making another.
func executeMeeting(ctx context.Context, s *Server, a store.Approval) (json.RawMessage, error) {
	if s.Collab == nil {
		return nil, errors.New("integrations are not configured on this server")
	}
	svc, err := s.Collab.For(ctx, a.TenantID.String())
	if err != nil {
		return nil, err
	}
	out, err := collabops.Execute(ctx, svc, s.now(), a.Payload, a.ID.String())
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

// ───────────────────────────── connections (founders) ─────────────────────────────

type connectionSvc struct{ *Server }

func connView(c store.Connection) *connectionv1.Connection {
	return &connectionv1.Connection{Provider: c.Provider, Status: c.Status, Config: c.Config, ConnectedBy: c.ConnectedBy, UpdatedAt: c.UpdatedAt.Format("2006-01-02T15:04:05Z")}
}

func (s *connectionSvc) List(ctx context.Context, req *connect.Request[connectionv1.ListRequest]) (*connect.Response[connectionv1.ListResponse], error) {
	_, tenant, err := s.authorize(ctx, req.Msg.TenantId, roleApprover)
	if err != nil {
		return nil, err
	}
	resp := &connectionv1.ListResponse{}
	for _, p := range collabsvc.Providers {
		creds, settings, _ := collabsvc.Fields(p)
		resp.Providers = append(resp.Providers, &connectionv1.ProviderFields{Provider: p, Credentials: creds, Settings: settings})
	}
	if s.Collab == nil {
		return connect.NewResponse(resp), nil // nothing can be connected on this server; the form list still renders
	}
	cs, err := s.Collab.List(ctx, tenant)
	if err != nil {
		return nil, s.internal("connections: list", err)
	}
	for _, c := range cs {
		resp.Connections = append(resp.Connections, connView(c))
	}
	return connect.NewResponse(resp), nil
}

func (s *connectionSvc) Connect(ctx context.Context, req *connect.Request[connectionv1.ConnectRequest]) (*connect.Response[connectionv1.ConnectResponse], error) {
	p, tenant, err := s.authorize(ctx, req.Msg.TenantId, roleFounder)
	if err != nil {
		return nil, err
	}
	if s.Collab == nil {
		return nil, s.noIntegrations()
	}
	if err := s.Collab.Connect(ctx, tenant, req.Msg.Provider, req.Msg.Config, req.Msg.Credentials, p.Sub); err != nil {
		// Credential problems are for the person who typed them; say so without echoing anything they typed.
		if errors.Is(err, collab.ErrAuth) {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s rejected those credentials: %s", req.Msg.Provider, trimPrefix(err, "collab: provider rejected the credentials: ")))
		}
		return nil, s.collabErr("connect", err)
	}
	s.auditUser(ctx, tenant, p.Sub, "connection.connect", "connection", req.Msg.Provider, map[string]any{"provider": req.Msg.Provider})
	cs, err := s.Collab.List(ctx, tenant)
	if err != nil {
		return nil, s.internal("connections: list", err)
	}
	for _, c := range cs {
		if c.Provider == req.Msg.Provider {
			return connect.NewResponse(&connectionv1.ConnectResponse{Connection: connView(c)}), nil
		}
	}
	return connect.NewResponse(&connectionv1.ConnectResponse{}), nil
}

func (s *connectionSvc) Disconnect(ctx context.Context, req *connect.Request[connectionv1.DisconnectRequest]) (*connect.Response[connectionv1.DisconnectResponse], error) {
	p, tenant, err := s.authorize(ctx, req.Msg.TenantId, roleFounder)
	if err != nil {
		return nil, err
	}
	if s.Collab == nil {
		return nil, s.noIntegrations()
	}
	if err := s.Collab.Disconnect(ctx, tenant, req.Msg.Provider); err != nil {
		return nil, s.collabErr("disconnect", err)
	}
	s.auditUser(ctx, tenant, p.Sub, "connection.disconnect", "connection", req.Msg.Provider, map[string]any{"provider": req.Msg.Provider})
	return connect.NewResponse(&connectionv1.DisconnectResponse{}), nil
}

// ───────────────────────────── controls ─────────────────────────────

type controlsSvc struct{ *Server }

func capView(c store.SpendCap) *controlsv1.SpendCap {
	return &controlsv1.SpendCap{Currency: c.Currency, PerTransactionMinor: c.PerTransaction, PerHourMinor: c.PerHour, PerDayMinor: c.PerDay,
		UpdatedBy: c.UpdatedBy, UpdatedAt: c.UpdatedAt.Format("2006-01-02T15:04:05Z")}
}

func (s *controlsSvc) GetSpendCaps(ctx context.Context, req *connect.Request[controlsv1.GetSpendCapsRequest]) (*connect.Response[controlsv1.GetSpendCapsResponse], error) {
	_, tenant, err := s.authorize(ctx, req.Msg.TenantId, roleApprover)
	if err != nil {
		return nil, err
	}
	caps, err := s.Repo.SpendCaps(ctx, tenant)
	if err != nil {
		return nil, s.internal("controls: caps", err)
	}
	resp := &controlsv1.GetSpendCapsResponse{}
	for _, c := range caps {
		resp.Caps = append(resp.Caps, capView(c))
	}
	return connect.NewResponse(resp), nil
}

func (s *controlsSvc) SetSpendCap(ctx context.Context, req *connect.Request[controlsv1.SetSpendCapRequest]) (*connect.Response[controlsv1.SetSpendCapResponse], error) {
	p, tenant, err := s.authorize(ctx, req.Msg.TenantId, roleFounder)
	if err != nil {
		return nil, err
	}
	c := req.Msg.Cap
	if c == nil || len(c.Currency) != 3 || strings.ToUpper(c.Currency) != c.Currency {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cap.currency must be a 3-letter uppercase currency code"))
	}
	for name, v := range map[string]*int64{"per_transaction_minor": c.PerTransactionMinor, "per_hour_minor": c.PerHourMinor, "per_day_minor": c.PerDayMinor} {
		if v != nil && (*v < 0 || *v > 1_000_000_000_000_000) {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s must be between 0 and 10^15", name))
		}
	}
	// The audit row (with what the cap was and what it became) is written in the same transaction as the change.
	err = s.Repo.SetSpendCap(ctx, tenant, store.SpendCap{Currency: c.Currency, PerTransaction: c.PerTransactionMinor, PerHour: c.PerHourMinor, PerDay: c.PerDayMinor, UpdatedBy: p.Sub})
	if errors.Is(err, store.ErrConflict) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("caps must satisfy per-transaction <= per-hour <= per-day"))
	}
	if err != nil {
		return nil, s.internal("controls: set cap", err)
	}
	caps, err := s.Repo.SpendCaps(ctx, tenant)
	if err != nil {
		return nil, s.internal("controls: caps", err)
	}
	resp := &controlsv1.SetSpendCapResponse{}
	for _, x := range caps {
		resp.Caps = append(resp.Caps, capView(x))
	}
	return connect.NewResponse(resp), nil
}

func (s *controlsSvc) VerifyAudit(ctx context.Context, req *connect.Request[controlsv1.VerifyAuditRequest]) (*connect.Response[controlsv1.VerifyAuditResponse], error) {
	_, tenant, err := s.authorize(ctx, req.Msg.TenantId, roleApprover)
	if err != nil {
		return nil, err
	}
	v, err := s.Repo.VerifyAudit(ctx, tenant)
	if err != nil {
		return nil, s.internal("controls: verify audit", err)
	}
	return connect.NewResponse(&controlsv1.VerifyAuditResponse{Intact: v.Intact, FirstBadId: v.FirstBadID, RowsChecked: v.RowsChecked, HeadHex: hex.EncodeToString(v.Head)}), nil
}

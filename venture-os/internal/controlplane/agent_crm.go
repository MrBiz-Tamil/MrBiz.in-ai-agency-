package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	agentv1 "github.com/mrbiz/venture-os/gen/venture/agent/v1"
	crmv1 "github.com/mrbiz/venture-os/gen/venture/crm/v1"
	"github.com/mrbiz/venture-os/internal/crm"
	"github.com/mrbiz/venture-os/internal/crmops"
)

const maxCRMArgsBytes = 64 << 10

// runCRM executes one crmops operation on a tenant's CRM and returns its JSON answer and the id of the record
// written. It is the single path both callers (agents by token, people by login) go through, so validation, size
// limits and error wording are identical. The caller has already authenticated and authorised the actor.
func (s *Server) runCRM(ctx context.Context, tenant uuid.UUID, op crmops.Op, actor string, args []byte) (out []byte, entityID string, err error) {
	if s.CRM == nil {
		return nil, "", connect.NewError(connect.CodeFailedPrecondition, errors.New("this business has no CRM connected"))
	}
	if len(args) > maxCRMArgsBytes {
		return nil, "", connect.NewError(connect.CodeInvalidArgument, errors.New("arguments are too large"))
	}
	c, err := s.CRM.For(ctx, tenant)
	if err != nil {
		return nil, "", s.internal("crm provider", err)
	}
	res, err := op.Run(ctx, c, actor, args)
	if err != nil {
		return nil, "", s.crmErr(op.Name, err)
	}
	out, err = json.Marshal(res.Data)
	if err != nil {
		return nil, "", s.internal("crm result", err)
	}
	return out, res.EntityID, nil
}

// CRMOperation runs one crm_ops operation on the calling agent's tenant. The tenant comes from the token only; the
// operations (and their policy) live in internal/crmops so the schema an agent was shown is exactly what runs here.
func (s *agentSvc) CRMOperation(ctx context.Context, req *connect.Request[agentv1.CRMOperationRequest]) (*connect.Response[agentv1.CRMOperationResponse], error) {
	claims, t, _, err := s.session(ctx)
	if err != nil {
		return nil, err
	}
	op, ok := crmops.Find(req.Msg.Operation)
	if !ok || op.HumanOnly { // human-only operations do not exist as far as an agent can tell
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("unknown CRM operation"))
	}
	who := requester(claims)
	out, entity, err := s.runCRM(ctx, claims.TenantID, op, who, req.Msg.ArgsJson)
	if err != nil {
		return nil, err
	}
	// What an outsider typed in a note or task must not reach the agent as raw instructions or raw secrets.
	if out, err = s.screenForAgent(ctx, t.ID, "crm", out); err != nil {
		return nil, s.internal("crm privacy screen", err)
	}
	if !op.ReadOnly {
		// Who did what, never the business data itself.
		if err := s.Repo.AuditActor(ctx, t.ID, "AGENT", who, "crm."+op.Name, "crm", entity, map[string]any{}); err != nil {
			s.log().Error("audit write failed", "action", "crm."+op.Name, "err", err)
		}
	}
	return connect.NewResponse(&agentv1.CRMOperationResponse{ResultJson: out}), nil
}

type crmSvc struct{ *Server }

// Execute runs one CRM operation as the signed-in user. Reads need the viewer role; every write needs approver or
// founder. A caller who is not a member of the business gets NotFound, as everywhere else, so ids cannot be probed.
func (s *crmSvc) Execute(ctx context.Context, req *connect.Request[crmv1.ExecuteRequest]) (*connect.Response[crmv1.ExecuteResponse], error) {
	op, ok := crmops.Find(req.Msg.Operation)
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("unknown CRM operation"))
	}
	min := roleApprover
	if op.ReadOnly {
		min = roleViewer
	}
	p, tenant, err := s.authorize(ctx, req.Msg.TenantId, min)
	if err != nil {
		return nil, err
	}
	who := "user:" + p.Sub
	out, entity, err := s.runCRM(ctx, tenant, op, who, req.Msg.ArgsJson)
	if err != nil {
		return nil, err
	}
	if !op.ReadOnly {
		if err := s.Repo.AuditUser(ctx, tenant, p.Sub, "crm."+op.Name, "crm", entity, map[string]any{}); err != nil {
			s.log().Error("audit write failed", "action", "crm."+op.Name, "err", err)
		}
	}
	return connect.NewResponse(&crmv1.ExecuteResponse{ResultJson: out}), nil
}

// crmErr maps CRM errors to codes whose messages are shown verbatim to the agent or user; anything unexpected is
// generic.
func (s *Server) crmErr(what string, err error) error {
	switch {
	case errors.Is(err, crm.ErrInvalid), errors.Is(err, crm.ErrIdempotencyMismatch):
		return connect.NewError(connect.CodeInvalidArgument, plain(err))
	case errors.Is(err, crm.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, plain(err))
	case errors.Is(err, crm.ErrConflict):
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("the record changed since you read it; fetch it again and retry with its new version"))
	case errors.Is(err, crm.ErrUnsupported):
		return connect.NewError(connect.CodeFailedPrecondition, plain(err))
	}
	return s.internal("crm "+what, err)
}

// plain drops the package prefix the CRM errors carry ("crm: invalid input: ..."), which means nothing to a reader.
func plain(err error) error {
	msg := err.Error()
	for _, p := range []string{"crm: invalid input: ", "crm: not found: ", "crm: idempotency key reused with different input: ", "crm: not supported by this backend: "} {
		if rest, ok := strings.CutPrefix(msg, p); ok {
			if p == "crm: not found: " {
				return errors.New("not found: " + rest)
			}
			return errors.New(rest)
		}
	}
	return err
}

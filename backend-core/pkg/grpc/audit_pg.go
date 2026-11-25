package grpc

import (
	"context"
	"encoding/json"

	"github.com/mrbiz/abos/backend-core/pkg/db/repository"
)

// PostgresAuditSink persists Decision X-Ray records through the repository.
type PostgresAuditSink struct {
	Repo *repository.Repository
}

func (p *PostgresAuditSink) Record(ctx context.Context, e AuditEntry) (int64, error) {
	var tool *string
	if e.ToolName != "" {
		t := e.ToolName
		tool = &t
	}
	var in, out json.RawMessage
	if e.Input != "" {
		in = asJSON(e.Input)
	}
	if e.Output != "" {
		out = asJSON(e.Output)
	}
	res, err := p.Repo.CreateAuditLog(ctx, repository.AuditXRayLog{
		WorkspaceID:     e.WorkspaceID,
		TicketID:        e.TicketID,
		StepNumber:      e.StepNumber,
		ToolName:        tool,
		InputSanitized:  in,
		OutputSanitized: out,
		PolicyDecision:  e.PolicyDecision,
		TokenCost:       e.TokenCost,
		LatencyMs:       e.LatencyMs,
	})
	if err != nil {
		return 0, err
	}
	return res.ID, nil
}

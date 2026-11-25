package mcpbus

import (
	"context"
	"encoding/json"

	agentv1 "github.com/mrbiz/venture-os/gen/venture/agent/v1"
	"github.com/mrbiz/venture-os/internal/collabops"
)

// collabTools exposes every collabops operation as an MCP tool (meetings_ops and notes_ops). As with crm_ops the bus
// forwards the arguments untouched: the control plane decodes them strictly, checks the token and applies the tenant.
//
// These tools are offered when the deployment has integrations enabled (Server.EnableCollab). Whether THIS business
// has connected Google, Zoom or Notion is a per-business fact the bus cannot know, so a call for a provider that is not
// connected fails with a plain message telling the agent to ask the founder to connect it.
func collabTools() []tool {
	ops := collabops.All()
	out := make([]tool, 0, len(ops))
	for _, o := range ops {
		o := o
		out = append(out, tool{
			Name: o.Name, Family: o.Family, ReadOnly: o.ReadOnly, Description: o.Description, Schema: o.Schema,
			run: func(ctx context.Context, c *call, raw json.RawMessage) (any, error) {
				r, err := c.agent.CollabOperation(ctx, authed(c, &agentv1.CollabOperationRequest{Operation: o.Name, ArgsJson: raw}))
				if err != nil {
					return nil, err
				}
				return rawJSON(r.Msg.ResultJson), nil
			},
		})
	}
	return out
}

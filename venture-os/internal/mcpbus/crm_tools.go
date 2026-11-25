package mcpbus

import (
	"context"
	"encoding/json"

	agentv1 "github.com/mrbiz/venture-os/gen/venture/agent/v1"
	"github.com/mrbiz/venture-os/internal/crmops"
)

// crmTools exposes every crmops operation as an MCP tool. The bus does not interpret the arguments: the control plane
// decodes them strictly against the same operation definition, authenticates the token, and applies the tenant. Here
// they are forwarded as the agent sent them.
//
// These tools exist only when the business has a CRM behind the control plane (Server.EnableCRM). Otherwise they are
// absent from tools/list, so an agent is never offered a tool that cannot work.
func crmTools() []tool {
	ops := crmops.ForAgents()
	out := make([]tool, 0, len(ops))
	for _, o := range ops {
		o := o
		out = append(out, tool{
			Name: o.Name, Family: crmops.Family, ReadOnly: o.ReadOnly, Description: o.Description, Schema: o.Schema,
			run: func(ctx context.Context, c *call, raw json.RawMessage) (any, error) {
				r, err := c.agent.CRMOperation(ctx, authed(c, &agentv1.CRMOperationRequest{Operation: o.Name, ArgsJson: raw}))
				if err != nil {
					return nil, err
				}
				return rawJSON(r.Msg.ResultJson), nil
			},
		})
	}
	return out
}

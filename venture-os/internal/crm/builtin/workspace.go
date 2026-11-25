package builtin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/ports"
)

// Workspace adapts the built-in CRM to the provisioner's ports.CRM, so a tenant whose CRM is built in is provisioned
// by the same saga as one that uses an external CRM.
//
// There is nothing to create: the tenant's CRM tables already exist, and the "db.seed" stage (stage 1) has written
// the blueprint's pipelines into crm_pipelines before the CRM steps run. The steps therefore VERIFY rather than
// create, which is what makes them worth running: if the pipelines the CRM would serve differ from the approved
// blueprint, provisioning fails loudly instead of leaving a sales team on the wrong pipeline.
type Workspace struct{ p *Provider }

var _ ports.CRM = (*Workspace)(nil)

// NewWorkspace returns the provisioner adapter for p.
func NewWorkspace(p *Provider) *Workspace { return &Workspace{p: p} }

func workspaceRef(t ports.TenantInfo) ports.Ref { return ports.Ref("builtin:" + t.ID.String()) }

// EnsureWorkspace confirms the tenant exists and names its (virtual) workspace.
func (w *Workspace) EnsureWorkspace(ctx context.Context, t ports.TenantInfo) (ports.Ref, error) {
	if _, err := w.p.For(ctx, t.ID); err != nil {
		return "", err
	}
	return workspaceRef(t), nil
}

// ApplyPipelines checks that the CRM serves exactly the blueprint's pipelines, in order.
func (w *Workspace) ApplyPipelines(ctx context.Context, t ports.TenantInfo, ws ports.Ref, want []domain.Pipeline) error {
	if ws != workspaceRef(t) {
		return fmt.Errorf("builtin crm: workspace %q is not tenant %s's", ws, t.ID)
	}
	c, err := w.p.For(ctx, t.ID)
	if err != nil {
		return err
	}
	have, err := c.Pipelines(ctx)
	if err != nil {
		return err
	}
	if len(have) != len(want) {
		return fmt.Errorf("builtin crm: the tenant has %d active pipelines but the blueprint defines %d", len(have), len(want))
	}
	for i := range want {
		if canon(have[i]) != canon(want[i]) {
			return fmt.Errorf("builtin crm: pipeline %q differs from the blueprint (position %d)", want[i].Key, i)
		}
	}
	return nil
}

// RemoveWorkspace does nothing, on purpose. A failed provisioning run is compensated before the tenant has any CRM
// data, and deleting a business's customers is not something a rollback step should be able to do: tenant data is
// removed only by an explicit, separate operation.
func (w *Workspace) RemoveWorkspace(context.Context, ports.TenantInfo, ports.Ref) error { return nil }

// canon is a comparable form of a pipeline (nil and empty slices are the same).
func canon(p domain.Pipeline) string {
	if p.Stages == nil {
		p.Stages = []domain.Stage{}
	}
	if p.CustomFields == nil {
		p.CustomFields = []domain.CustomField{}
	}
	b, _ := json.Marshal(p)
	return string(b)
}

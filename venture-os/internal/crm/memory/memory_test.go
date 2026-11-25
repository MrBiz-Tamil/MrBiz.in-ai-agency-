package memory_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/crm"
	"github.com/mrbiz/venture-os/internal/crm/crmtest"
	"github.com/mrbiz/venture-os/internal/crm/memory"
)

type backend struct{ *memory.Store }

func (b backend) NewTenant(t *testing.T, ps []crm.Pipeline) uuid.UUID {
	t.Helper()
	id := uuid.New()
	b.AddTenant(id, ps)
	return id
}

// The reference backend passes the contract with every capability on...
func TestContract(t *testing.T) {
	crmtest.Run(t, func(t *testing.T) crmtest.Backend { return backend{memory.New(memory.Options{})} })
}

// ...and with the optional ones off, which exercises the suite's capability-adaptive branches (an adapter for a CRM
// without versions or stage history must refuse ifVersion and DealHistory rather than ignore them).
func TestContractWithoutOptionalCapabilities(t *testing.T) {
	crmtest.Run(t, func(t *testing.T) crmtest.Backend {
		return backend{memory.New(memory.Options{NoOptimisticConcurrency: true, NoStageHistory: true})}
	})
}

func TestUnconfiguredTenantHasNoCRM(t *testing.T) {
	if _, err := memory.New(memory.Options{}).For(t.Context(), uuid.New()); err == nil {
		t.Fatal("a tenant that was never configured must not get a CRM")
	}
}

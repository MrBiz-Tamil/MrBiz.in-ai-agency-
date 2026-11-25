package fakes

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/ports"
	"github.com/mrbiz/venture-os/internal/provisioner"
)

// MemStore is an in-memory provisioner.Store with the same run semantics as the Postgres store: one run per
// (tenant, idempotency key), lease-based takeover of stale runs, retire-instead-of-delete seeding.
type MemStore struct {
	hub *Hub

	mu           sync.Mutex
	Now          func() time.Time // injectable clock
	runs         map[string]*memRun
	steps        map[uuid.UUID]map[string]provisioner.StepRecord
	seeded       map[uuid.UUID]bool
	skills       map[uuid.UUID][]ports.Skill
	status       map[uuid.UUID]string
	statusLog    map[uuid.UUID][]string
	integrations map[uuid.UUID]map[string]ports.Ref
	audits       []string
}

type memRun struct {
	provisioner.Run
	updated time.Time
}

func NewMemStore(h *Hub) *MemStore {
	return &MemStore{
		hub: h, Now: time.Now,
		runs:         map[string]*memRun{},
		steps:        map[uuid.UUID]map[string]provisioner.StepRecord{},
		seeded:       map[uuid.UUID]bool{},
		skills:       map[uuid.UUID][]ports.Skill{},
		status:       map[uuid.UUID]string{},
		statusLog:    map[uuid.UUID][]string{},
		integrations: map[uuid.UUID]map[string]ports.Ref{},
	}
}

func runKey(tenant uuid.UUID, key string) string { return tenant.String() + "/" + key }

func (m *MemStore) BeginRun(ctx context.Context, tenant, blueprint uuid.UUID, key string, lease time.Duration) (provisioner.Run, bool, error) {
	if err := m.hub.enter(ctx, "store", "BeginRun", tenant); err != nil {
		return provisioner.Run{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := runKey(tenant, key)
	if r, ok := m.runs[k]; ok {
		switch r.Status {
		case provisioner.RunPending, provisioner.RunRunning, provisioner.RunCompensating:
			if m.Now().Sub(r.updated) < lease {
				return r.Run, false, nil
			}
			r.updated = m.Now() // stale: take over
			return r.Run, true, nil
		default:
			return r.Run, false, nil
		}
	}
	// Only one in-flight run per tenant (mirrors the partial unique index).
	for _, r := range m.runs {
		if r.TenantID == tenant {
			switch r.Status {
			case provisioner.RunPending, provisioner.RunRunning, provisioner.RunCompensating:
				if m.Now().Sub(r.updated) < lease {
					return provisioner.Run{}, false, fmt.Errorf("tenant %s already has an active provisioning run", tenant)
				}
			}
		}
	}
	r := &memRun{Run: provisioner.Run{ID: uuid.New(), TenantID: tenant, BlueprintID: blueprint, Status: provisioner.RunPending}, updated: m.Now()}
	m.runs[k] = r
	m.steps[r.ID] = map[string]provisioner.StepRecord{}
	return r.Run, true, nil
}

func (m *MemStore) find(run uuid.UUID) *memRun {
	for _, r := range m.runs {
		if r.ID == run {
			return r
		}
	}
	return nil
}

func (m *MemStore) Steps(ctx context.Context, tenant, run uuid.UUID) ([]provisioner.StepRecord, error) {
	if err := m.hub.enter(ctx, "store", "Steps", tenant); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]provisioner.StepRecord, 0, len(m.steps[run]))
	for _, s := range m.steps[run] {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })
	return out, nil
}

func (m *MemStore) SaveStep(ctx context.Context, tenant, run uuid.UUID, s provisioner.StepRecord) error {
	if err := m.hub.enter(ctx, "store", "SaveStep", tenant); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.steps[run] == nil {
		return fmt.Errorf("unknown run %s", run)
	}
	m.steps[run][s.Key] = s
	if r := m.find(run); r != nil {
		r.updated = m.Now()
	}
	return nil
}

func (m *MemStore) SetRunStatus(ctx context.Context, tenant, run uuid.UUID, status provisioner.RunStatus) error {
	if err := m.hub.enter(ctx, "store", "SetRunStatus", tenant); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.find(run); r != nil {
		r.Status, r.updated = status, m.Now()
	}
	return nil
}

func (m *MemStore) FinishRun(ctx context.Context, tenant, run uuid.UUID, status provisioner.RunStatus, errMsg string) error {
	if err := m.hub.enter(ctx, "store", "FinishRun", tenant); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.find(run); r != nil {
		r.Status, r.Error, r.updated = status, errMsg, m.Now()
	}
	return nil
}

func (m *MemStore) SeedTenant(ctx context.Context, tenant, _ uuid.UUID, _ domain.Blueprint, skills []ports.Skill) error {
	if err := m.hub.enter(ctx, "store", "SeedTenant", tenant); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seeded[tenant] = true
	m.skills[tenant] = append([]ports.Skill(nil), skills...)
	return nil
}

func (m *MemStore) RetireSeed(ctx context.Context, tenant uuid.UUID) error {
	if err := m.hub.enter(ctx, "store", "RetireSeed", tenant); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seeded[tenant] = false
	return nil
}

func (m *MemStore) SetTenantStatus(ctx context.Context, tenant uuid.UUID, status string) error {
	if err := m.hub.enter(ctx, "store", "SetTenantStatus", tenant); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status[tenant] = status
	m.statusLog[tenant] = append(m.statusLog[tenant], status)
	return nil
}

func (m *MemStore) RecordIntegrations(ctx context.Context, tenant uuid.UUID, refs map[string]ports.Ref) error {
	if err := m.hub.enter(ctx, "store", "RecordIntegrations", tenant); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.integrations[tenant] = refs
	return nil
}

func (m *MemStore) Audit(_ context.Context, tenant uuid.UUID, action string, _ map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audits = append(m.audits, tenant.String()+":"+action)
	return nil
}

// ── inspection helpers for tests ──

func (m *MemStore) TenantStatus(t uuid.UUID) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status[t]
}
func (m *MemStore) StatusHistory(t uuid.UUID) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.statusLog[t]...)
}
func (m *MemStore) Seeded(t uuid.UUID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seeded[t]
}
func (m *MemStore) Integrations(t uuid.UUID) map[string]ports.Ref {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.integrations[t]
}
func (m *MemStore) Audits() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.audits...)
}

// Run returns the run for (tenant, key).
func (m *MemStore) Run(tenant uuid.UUID, key string) (provisioner.Run, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runKey(tenant, key)]
	if !ok {
		return provisioner.Run{}, false
	}
	return r.Run, true
}

// Advance moves the store's clock forward (to age a run past its lease).
func (m *MemStore) Advance(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.Now()
	m.Now = func() time.Time { return now.Add(d) }
}

var _ provisioner.Store = (*MemStore)(nil)

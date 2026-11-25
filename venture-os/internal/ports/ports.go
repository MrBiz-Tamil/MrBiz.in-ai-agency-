// Package ports defines the interfaces through which Venture OS talks to every external system.
//
// The provisioner, the MCP bus and the finance engine depend only on these interfaces. Real adapters
// (internal/connectors/*) and in-memory fakes (internal/fakes) both implement them, so the whole control plane is
// testable without Keycloak, Twenty, LibreDesk, ERPNext, Hermes or an LLM.
//
// CONTRACT FOR EVERY MUTATING METHOD
//   - Idempotent: calling it again with the same tenant and equivalent input succeeds and changes nothing. The
//     provisioner retries freely, so "already exists" is success, not an error.
//   - Tenant-scoped: the tenant id is always an explicit argument; adapters never infer it from ambient state.
//   - Returns a stable external reference that is persisted in tenant_integrations / provision_steps and later
//     handed to the matching Remove* call during compensation.
//   - Remove* methods are idempotent too: removing something that is already gone is success.
//   - Errors wrapped with ErrTransient are retried with backoff; every other error fails the step.
package ports

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/domain"
)

// ErrTransient marks a failure worth retrying (timeouts, 429/5xx). Wrap it: fmt.Errorf("x: %w", ErrTransient).
var ErrTransient = errors.New("transient failure")

// Ref is an opaque external identifier returned by an adapter (e.g. an ERPNext company name, a Twenty workspace id).
type Ref string

// TenantInfo is the identity of the tenant being provisioned.
type TenantInfo struct {
	ID   uuid.UUID
	Slug string
	Name string
}

// IdentityProvider maps a tenant to an organization in the OIDC provider (Keycloak Organizations).
type IdentityProvider interface {
	EnsureOrganization(ctx context.Context, t TenantInfo, founderSub string) (Ref, error)
	RemoveOrganization(ctx context.Context, t TenantInfo, ref Ref) error
}

// ERP provisions the accounting side (ERPNext Company, chart of accounts, tax templates).
type ERP interface {
	EnsureCompany(ctx context.Context, t TenantInfo, profile domain.BusinessProfile, acct domain.Accounting) (Ref, error)
	RemoveCompany(ctx context.Context, t TenantInfo, ref Ref) error
}

// CRM provisions a Twenty workspace and its pipelines / custom fields.
type CRM interface {
	EnsureWorkspace(ctx context.Context, t TenantInfo) (Ref, error)
	ApplyPipelines(ctx context.Context, t TenantInfo, ws Ref, pipelines []domain.Pipeline) error
	RemoveWorkspace(ctx context.Context, t TenantInfo, ref Ref) error
}

// Support provisions LibreDesk inboxes (channels) and the seeded knowledge base.
type Support interface {
	EnsureInboxes(ctx context.Context, t TenantInfo, channels []domain.Channel) (Ref, error)
	SeedKnowledgeBase(ctx context.Context, t TenantInfo, inbox Ref, docs []domain.KBDoc) error
	RemoveInboxes(ctx context.Context, t TenantInfo, ref Ref) error
}

// Skill is one rendered, hashed SKILL.md destined for a Hermes profile.
type Skill struct {
	AgentKey string
	Markdown string
	SHA256   [32]byte
}

// AgentRuntime provisions the Hermes side: a profile bound to the tenant's MCP token, and its skills.
type AgentRuntime interface {
	EnsureProfile(ctx context.Context, t TenantInfo, mcpToken string) (Ref, error)
	InstallSkills(ctx context.Context, t TenantInfo, profile Ref, skills []Skill) error
	RemoveProfile(ctx context.Context, t TenantInfo, ref Ref) error
}

// Message is one turn of an LLM conversation.
type Message struct {
	Role    string // "system" | "user" | "assistant"
	Content string
}

// LLMRequest is a single completion request. Schema, when set, asks for a JSON object matching that JSON Schema.
type LLMRequest struct {
	Model     string
	Messages  []Message
	Schema    []byte
	MaxTokens int
}

// LLMResponse is the model's reply.
type LLMResponse struct {
	Content      string
	InputTokens  int
	OutputTokens int
}

// LLM is the only way Genesis reaches a language model.
type LLM interface {
	Complete(ctx context.Context, req LLMRequest) (LLMResponse, error)
}

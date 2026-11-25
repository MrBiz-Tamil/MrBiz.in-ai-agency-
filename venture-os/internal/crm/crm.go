// Package crm defines the CRM interface every sales backend implements: Venture OS's built-in CRM, and adapters for
// external CRMs (Twenty, HubSpot, Salesforce, Zoho, Odoo, ...).
//
// The rest of the system (quote flow, agent tools, portfolio) talks only to this interface. The contract test suite
// in internal/crm/crmtest defines what "correct" means; a backend is only trusted once it passes it.
//
// DESIGN RULES (they exist so external adapters stay possible):
//
//   - Tenant-bound. Provider.For returns a CRM bound to one tenant; every call through it is scoped to that tenant.
//     There is no tenant argument to get wrong.
//   - Portable fields only. Each entity carries what every CRM has, plus a bounded map of custom text fields. Anything
//     CRM-specific stays inside the adapter.
//   - Idempotent creates. Every create takes an idempotency key: replaying it returns the original record, reusing it
//     for a different request is an error. External CRMs have no such feature, so adapters must provide it.
//   - Optimistic concurrency where the backend can offer it (Capabilities.OptimisticConcurrency): updates may carry the
//     version they were based on, and lose with ErrConflict if the record changed.
//   - Archive, never hard delete. Archived records disappear from reads and lists but keep their history.
//   - Stages are data. A deal's pipeline and stage come from the tenant's blueprint (Pipelines); the interface does not
//     know "Quoted" means anything. Business rules about moving stages belong above this interface.
//   - Money is int64 minor units plus an ISO-4217 currency, as everywhere in Venture OS.
package crm

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/domain"
)

// ID is a backend-native, opaque record identifier. Callers must not parse it.
type ID string

// Errors returned by every backend. Backends wrap them with detail; callers use errors.Is.
var (
	// ErrNotFound: the record does not exist, is archived, or belongs to another tenant (indistinguishable on purpose).
	ErrNotFound = errors.New("crm: not found")
	// ErrInvalid: the input breaks a rule (see Validate*). The message says which.
	ErrInvalid = errors.New("crm: invalid input")
	// ErrConflict: the record changed since the version the caller based the update on.
	ErrConflict = errors.New("crm: version conflict")
	// ErrIdempotencyMismatch: the idempotency key was already used for a different request.
	ErrIdempotencyMismatch = errors.New("crm: idempotency key reused with different input")
	// ErrUnsupported: the backend does not offer the requested capability.
	ErrUnsupported = errors.New("crm: not supported by this backend")
)

// Limits shared by every backend.
const (
	MaxNameLen        = 200
	MaxTextLen        = 2000
	MaxBodyLen        = 20000
	MaxFields         = 50
	MaxPageSize       = 200
	DefaultPageSize   = 50
	MaxAmountMinor    = int64(1_000_000_000_000_000)
	MaxIdempotencyKey = 200
)

// Provider hands out tenant-bound CRM clients.
type Provider interface {
	// For returns the CRM for one tenant. It fails if the tenant has no CRM configured.
	For(ctx context.Context, tenant uuid.UUID) (CRM, error)
}

// Capabilities says what a backend offers beyond the mandatory core. The contract suite adapts to them.
type Capabilities struct {
	// OptimisticConcurrency: updates honour ifVersion and Version is maintained.
	OptimisticConcurrency bool
	// StageHistory: DealHistory returns every stage change.
	StageHistory bool
}

// Page is one page of results. Next is empty on the last page.
type Page[T any] struct {
	Items []T
	Next  string
}

// PageRequest selects a page. Size 0 means DefaultPageSize; Cursor is an opaque Next value from a previous page.
// Results are always ordered oldest-created first, and paging is stable while records are added.
type PageRequest struct {
	Size   int
	Cursor string
}

// ───────────────────────────── pipelines ─────────────────────────────

// Pipeline is the tenant's sales pipeline definition, from its blueprint.
type Pipeline = domain.Pipeline

// ───────────────────────────── companies ─────────────────────────────

type CompanyInput struct {
	Name     string
	Domain   string // e.g. "acme.com"; stored lower-case
	Industry string
	Country  string // ISO 3166-1 alpha-2, upper-case
	Website  string
	Phone    string
	OwnerSub string // OIDC subject of the owning user; empty = unassigned
	Fields   map[string]string
}

// CompanyPatch changes only the fields that are non-nil. Fields entries are merged; an empty value removes the key.
type CompanyPatch struct {
	Name, Domain, Industry, Country, Website, Phone, OwnerSub *string
	Fields                                                    map[string]string
}

type Company struct {
	ID        ID
	Name      string
	Domain    string
	Industry  string
	Country   string
	Website   string
	Phone     string
	OwnerSub  string
	Fields    map[string]string
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type CompanyFilter struct {
	Query           string // case-insensitive substring of name or domain; wildcards are literal
	OwnerSub        string
	IncludeArchived bool
}

// ───────────────────────────── contacts ─────────────────────────────

type ContactInput struct {
	CompanyID ID // optional
	FirstName string
	LastName  string
	Email     string // stored lower-case
	Phone     string
	Title     string
	OwnerSub  string
	Fields    map[string]string
}

// ContactPatch changes only non-nil fields. A contact's company cannot be changed after creation.
type ContactPatch struct {
	FirstName, LastName, Email, Phone, Title, OwnerSub *string
	Fields                                             map[string]string
}

type Contact struct {
	ID        ID
	CompanyID ID
	FirstName string
	LastName  string
	Email     string
	Phone     string
	Title     string
	OwnerSub  string
	Fields    map[string]string
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type ContactFilter struct {
	Query           string // name or email substring
	CompanyID       ID
	OwnerSub        string
	IncludeArchived bool
}

// ───────────────────────────── deals ─────────────────────────────

// Money is an optional amount; both fields are set together or not at all.
type Money struct {
	Minor    int64
	Currency string
}

type DealInput struct {
	Name          string
	CompanyID     ID // required
	ContactID     ID // optional
	Pipeline      string
	Stage         string // must be a stage of Pipeline
	Amount        *Money
	ExpectedClose time.Time // date only; zero = unset
	OwnerSub      string
	// CreatedBy is who is creating the deal ("user:<sub>" or "agent:<key>"); it is the author of the first stage-history
	// entry, so history says who started a deal as well as who moved it.
	CreatedBy string
	// Fields must use only the custom fields the pipeline defines, and set every one marked required.
	Fields map[string]string
}

// DealPatch changes only non-nil fields. Stage changes go through MoveDeal; pipeline and company are immutable.
// ClearAmount removes the amount; ClearExpectedClose removes the date.
type DealPatch struct {
	Name, OwnerSub     *string
	ContactID          *ID
	Amount             *Money
	ClearAmount        bool
	ExpectedClose      *time.Time
	ClearExpectedClose bool
	Fields             map[string]string
}

type Deal struct {
	ID             ID
	Name           string
	CompanyID      ID
	ContactID      ID
	Pipeline       string
	Stage          string
	Amount         *Money
	ExpectedClose  time.Time
	OwnerSub       string
	Fields         map[string]string
	StageChangedAt time.Time
	Version        int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type DealFilter struct {
	Query           string // name substring
	Pipeline        string
	Stage           string
	CompanyID       ID
	OwnerSub        string
	IncludeArchived bool
}

// StageChange is one entry of a deal's history, oldest first. The first entry has an empty From (creation).
type StageChange struct {
	From, To  string
	ChangedBy string
	ChangedAt time.Time
}

// ───────────────────────────── activities ─────────────────────────────

type ActivityKind string

const (
	ActivityNote    ActivityKind = "NOTE"
	ActivityCall    ActivityKind = "CALL"
	ActivityEmail   ActivityKind = "EMAIL"
	ActivityMeeting ActivityKind = "MEETING"
	ActivityTask    ActivityKind = "TASK"
)

type ActivityInput struct {
	Kind    ActivityKind
	Subject string
	Body    string
	// DueAt is only meaningful for TASK.
	DueAt time.Time
	// At least one of the three parents is required.
	CompanyID, ContactID, DealID ID
	OwnerSub                     string
	CreatedBy                    string
}

type Activity struct {
	ID          ID
	Kind        ActivityKind
	Subject     string
	Body        string
	DueAt       time.Time
	CompletedAt time.Time // zero until a TASK is completed
	CompanyID   ID
	ContactID   ID
	DealID      ID
	OwnerSub    string
	CreatedBy   string
	CreatedAt   time.Time
}

type ActivityFilter struct {
	CompanyID, ContactID, DealID ID
	Kind                         ActivityKind
	OpenTasksOnly                bool // TASKs that are not completed
	OwnerSub                     string
}

// ───────────────────────────── the interface ─────────────────────────────

// CRM is a tenant-bound sales backend.
type CRM interface {
	Capabilities() Capabilities

	// Pipelines returns the tenant's pipelines with stages in order.
	Pipelines(ctx context.Context) ([]Pipeline, error)

	// Creates are idempotent per key. created is false when an earlier call with the same key and input is replayed.
	CreateCompany(ctx context.Context, key string, in CompanyInput) (c Company, created bool, err error)
	GetCompany(ctx context.Context, id ID) (Company, error)
	// ifVersion 0 means "do not check".
	UpdateCompany(ctx context.Context, id ID, p CompanyPatch, ifVersion int64) (Company, error)
	ArchiveCompany(ctx context.Context, id ID, ifVersion int64) error
	ListCompanies(ctx context.Context, f CompanyFilter, p PageRequest) (Page[Company], error)

	CreateContact(ctx context.Context, key string, in ContactInput) (c Contact, created bool, err error)
	GetContact(ctx context.Context, id ID) (Contact, error)
	UpdateContact(ctx context.Context, id ID, p ContactPatch, ifVersion int64) (Contact, error)
	ArchiveContact(ctx context.Context, id ID, ifVersion int64) error
	ListContacts(ctx context.Context, f ContactFilter, p PageRequest) (Page[Contact], error)

	CreateDeal(ctx context.Context, key string, in DealInput) (d Deal, created bool, err error)
	GetDeal(ctx context.Context, id ID) (Deal, error)
	UpdateDeal(ctx context.Context, id ID, p DealPatch, ifVersion int64) (Deal, error)
	// MoveDeal changes the deal's stage. Moving to the current stage is a no-op (no version bump, no history entry).
	MoveDeal(ctx context.Context, id ID, toStage, by string, ifVersion int64) (Deal, error)
	ArchiveDeal(ctx context.Context, id ID, ifVersion int64) error
	ListDeals(ctx context.Context, f DealFilter, p PageRequest) (Page[Deal], error)
	// DealHistory requires Capabilities.StageHistory.
	DealHistory(ctx context.Context, id ID) ([]StageChange, error)

	CreateActivity(ctx context.Context, key string, in ActivityInput) (a Activity, created bool, err error)
	GetActivity(ctx context.Context, id ID) (Activity, error)
	// CompleteActivity marks a TASK done. Completing a completed task returns it unchanged. Other kinds are ErrInvalid.
	CompleteActivity(ctx context.Context, id ID, by string) (Activity, error)
	ListActivities(ctx context.Context, f ActivityFilter, p PageRequest) (Page[Activity], error)
}

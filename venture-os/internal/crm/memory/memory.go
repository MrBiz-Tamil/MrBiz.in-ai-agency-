// Package memory is the in-memory reference implementation of crm.CRM.
//
// It exists for three reasons: it is the executable specification the contract suite is debugged against first (a
// failure here is a suite or interface bug, not a database quirk); it backs fast tests of everything above the CRM
// (quote flow, agent tools); and it is the smallest correct example for anyone writing an external adapter.
//
// Nothing here is persisted. Options can switch off the optional capabilities so the suite's capability-adaptive
// branches are exercised too.
package memory

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/crm"
)

// Options turns optional capabilities off. The zero value offers everything.
type Options struct {
	NoOptimisticConcurrency bool // Version stays 0 and a non-zero ifVersion is ErrUnsupported
	NoStageHistory          bool // DealHistory is ErrUnsupported
}

// Store holds any number of tenants. It implements crm.Provider.
type Store struct {
	mu      sync.Mutex
	opts    Options
	tenants map[uuid.UUID]*tenantData
}

var _ crm.Provider = (*Store)(nil)

// New returns an empty store.
func New(opts Options) *Store {
	return &Store{opts: opts, tenants: map[uuid.UUID]*tenantData{}}
}

// AddTenant configures a tenant's CRM with its pipelines (provisioning does this in production). It replaces any
// earlier configuration for the tenant, data included.
func (s *Store) AddTenant(id uuid.UUID, pipelines []crm.Pipeline) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tenants[id] = newTenantData(pipelines)
}

// For returns the CRM of a configured tenant.
func (s *Store) For(ctx context.Context, tenant uuid.UUID) (crm.CRM, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	td, ok := s.tenants[tenant]
	if !ok {
		return nil, fmt.Errorf("memory crm: tenant %s has no CRM configured", tenant)
	}
	return &client{s: s, t: td}, nil
}

// ───────────────────────────── storage ─────────────────────────────

type row[T any] struct {
	seq      int64
	v        T
	archived bool
}

type table[T any] struct {
	byID  map[crm.ID]*row[T]
	order []*row[T] // ascending seq
}

func newTable[T any]() *table[T] { return &table[T]{byID: map[crm.ID]*row[T]{}} }

func (t *table[T]) insert(seq int64, id crm.ID, v T) *row[T] {
	r := &row[T]{seq: seq, v: v}
	t.byID[id] = r
	t.order = append(t.order, r)
	return r
}

// get finds a record; archived records are visible only when includeArchived is set.
func (t *table[T]) get(id crm.ID, includeArchived bool) (*row[T], bool) {
	r, ok := t.byID[id]
	if !ok || (r.archived && !includeArchived) {
		return nil, false
	}
	return r, true
}

type idemEntry struct {
	hash [32]byte
	id   crm.ID
}

type tenantData struct {
	pipelines []crm.Pipeline
	seq       int64
	companies *table[crm.Company]
	contacts  *table[crm.Contact]
	deals     *table[crm.Deal]
	acts      *table[crm.Activity]
	history   map[crm.ID][]crm.StageChange
	idem      map[string]idemEntry
}

func newTenantData(ps []crm.Pipeline) *tenantData {
	return &tenantData{
		pipelines: clonePipelines(ps),
		companies: newTable[crm.Company](),
		contacts:  newTable[crm.Contact](),
		deals:     newTable[crm.Deal](),
		acts:      newTable[crm.Activity](),
		history:   map[crm.ID][]crm.StageChange{},
		idem:      map[string]idemEntry{},
	}
}

func (td *tenantData) nextSeq() int64 { td.seq++; return td.seq }

// replay looks up an earlier create. Keys are scoped per entity kind (and, because td is per tenant, per tenant).
func (td *tenantData) replay(kind, key string, hash [32]byte) (id crm.ID, found bool, err error) {
	e, ok := td.idem[kind+"\x00"+key]
	if !ok {
		return "", false, nil
	}
	if e.hash != hash {
		return "", false, fmt.Errorf("%w: key %q was used for a different %s", crm.ErrIdempotencyMismatch, key, kind)
	}
	return e.id, true, nil
}

func (td *tenantData) remember(kind, key string, hash [32]byte, id crm.ID) {
	td.idem[kind+"\x00"+key] = idemEntry{hash: hash, id: id}
}

// ───────────────────────────── client ─────────────────────────────

type client struct {
	s *Store
	t *tenantData
}

func (c *client) Capabilities() crm.Capabilities {
	return crm.Capabilities{OptimisticConcurrency: !c.s.opts.NoOptimisticConcurrency, StageHistory: !c.s.opts.NoStageHistory}
}

// begin checks the context and takes the store lock; the returned func releases it.
func (c *client) begin(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.s.mu.Lock()
	return c.s.mu.Unlock, nil
}

func now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

func (c *client) firstVersion() int64 {
	if c.s.opts.NoOptimisticConcurrency {
		return 0
	}
	return 1
}

// checkVersion enforces ifVersion. A backend that cannot check must refuse rather than ignore.
func (c *client) checkVersion(ifVersion, current int64) error {
	if ifVersion == 0 {
		return nil
	}
	if c.s.opts.NoOptimisticConcurrency {
		return fmt.Errorf("%w: optimistic concurrency", crm.ErrUnsupported)
	}
	if ifVersion != current {
		return fmt.Errorf("%w: record is at version %d, caller had %d", crm.ErrConflict, current, ifVersion)
	}
	return nil
}

func (c *client) bump(v *int64, updated *time.Time) {
	if !c.s.opts.NoOptimisticConcurrency {
		*v++
	}
	*updated = now()
}

func (c *client) Pipelines(ctx context.Context) ([]crm.Pipeline, error) {
	unlock, err := c.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return clonePipelines(c.t.pipelines), nil
}

// ───────────────────────────── companies ─────────────────────────────

func (c *client) CreateCompany(ctx context.Context, key string, in crm.CompanyInput) (crm.Company, bool, error) {
	if err := ctx.Err(); err != nil {
		return crm.Company{}, false, err
	}
	if err := crm.ValidateKey(key); err != nil {
		return crm.Company{}, false, err
	}
	in, err := crm.NormalizeCompany(in)
	if err != nil {
		return crm.Company{}, false, err
	}
	unlock, err := c.begin(ctx)
	if err != nil {
		return crm.Company{}, false, err
	}
	defer unlock()
	hash := crm.HashInput(in)
	if id, found, err := c.t.replay("company", key, hash); err != nil {
		return crm.Company{}, false, err
	} else if found {
		r, _ := c.t.companies.get(id, true)
		return cloneCompany(r.v), false, nil
	}
	ts := now()
	co := crm.Company{
		ID: crm.ID(uuid.NewString()), Name: in.Name, Domain: in.Domain, Industry: in.Industry, Country: in.Country,
		Website: in.Website, Phone: in.Phone, OwnerSub: in.OwnerSub, Fields: in.Fields,
		Version: c.firstVersion(), CreatedAt: ts, UpdatedAt: ts,
	}
	c.t.companies.insert(c.t.nextSeq(), co.ID, co)
	c.t.remember("company", key, hash, co.ID)
	return cloneCompany(co), true, nil
}

func (c *client) GetCompany(ctx context.Context, id crm.ID) (crm.Company, error) {
	unlock, err := c.begin(ctx)
	if err != nil {
		return crm.Company{}, err
	}
	defer unlock()
	r, ok := c.t.companies.get(id, false)
	if !ok {
		return crm.Company{}, notFound("company", id)
	}
	return cloneCompany(r.v), nil
}

func (c *client) UpdateCompany(ctx context.Context, id crm.ID, p crm.CompanyPatch, ifVersion int64) (crm.Company, error) {
	if err := ctx.Err(); err != nil {
		return crm.Company{}, err
	}
	p, err := crm.NormalizeCompanyPatch(p)
	if err != nil {
		return crm.Company{}, err
	}
	unlock, err := c.begin(ctx)
	if err != nil {
		return crm.Company{}, err
	}
	defer unlock()
	r, ok := c.t.companies.get(id, false)
	if !ok {
		return crm.Company{}, notFound("company", id)
	}
	if err := c.checkVersion(ifVersion, r.v.Version); err != nil {
		return crm.Company{}, err
	}
	next := cloneCompany(r.v) // all changes go to the copy so a failure leaves the record untouched
	set(&next.Name, p.Name)
	set(&next.Domain, p.Domain)
	set(&next.Industry, p.Industry)
	set(&next.Country, p.Country)
	set(&next.Website, p.Website)
	set(&next.Phone, p.Phone)
	set(&next.OwnerSub, p.OwnerSub)
	if next.Fields, err = crm.MergeFields(next.Fields, p.Fields); err != nil {
		return crm.Company{}, err
	}
	c.bump(&next.Version, &next.UpdatedAt)
	r.v = next
	return cloneCompany(next), nil
}

func (c *client) ArchiveCompany(ctx context.Context, id crm.ID, ifVersion int64) error {
	unlock, err := c.begin(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	r, ok := c.t.companies.get(id, false)
	if !ok {
		return notFound("company", id)
	}
	if err := c.checkVersion(ifVersion, r.v.Version); err != nil {
		return err
	}
	r.archived = true
	c.bump(&r.v.Version, &r.v.UpdatedAt)
	return nil
}

func (c *client) ListCompanies(ctx context.Context, f crm.CompanyFilter, p crm.PageRequest) (crm.Page[crm.Company], error) {
	unlock, err := c.begin(ctx)
	if err != nil {
		return crm.Page[crm.Company]{}, err
	}
	defer unlock()
	return list(c.t.companies, f.IncludeArchived, func(x crm.Company) bool {
		return (contains(x.Name, f.Query) || contains(x.Domain, f.Query)) && (f.OwnerSub == "" || x.OwnerSub == f.OwnerSub)
	}, cloneCompany, p)
}

// ───────────────────────────── contacts ─────────────────────────────

func (c *client) CreateContact(ctx context.Context, key string, in crm.ContactInput) (crm.Contact, bool, error) {
	if err := ctx.Err(); err != nil {
		return crm.Contact{}, false, err
	}
	if err := crm.ValidateKey(key); err != nil {
		return crm.Contact{}, false, err
	}
	in, err := crm.NormalizeContact(in)
	if err != nil {
		return crm.Contact{}, false, err
	}
	in.CompanyID = crm.ID(strings.TrimSpace(string(in.CompanyID)))
	unlock, err := c.begin(ctx)
	if err != nil {
		return crm.Contact{}, false, err
	}
	defer unlock()
	hash := crm.HashInput(in)
	if id, found, err := c.t.replay("contact", key, hash); err != nil {
		return crm.Contact{}, false, err
	} else if found {
		r, _ := c.t.contacts.get(id, true)
		return cloneContact(r.v), false, nil
	}
	if err := c.requireCompany(in.CompanyID, false); err != nil {
		return crm.Contact{}, false, err
	}
	ts := now()
	ct := crm.Contact{
		ID: crm.ID(uuid.NewString()), CompanyID: in.CompanyID, FirstName: in.FirstName, LastName: in.LastName, Email: in.Email,
		Phone: in.Phone, Title: in.Title, OwnerSub: in.OwnerSub, Fields: in.Fields,
		Version: c.firstVersion(), CreatedAt: ts, UpdatedAt: ts,
	}
	c.t.contacts.insert(c.t.nextSeq(), ct.ID, ct)
	c.t.remember("contact", key, hash, ct.ID)
	return cloneContact(ct), true, nil
}

func (c *client) GetContact(ctx context.Context, id crm.ID) (crm.Contact, error) {
	unlock, err := c.begin(ctx)
	if err != nil {
		return crm.Contact{}, err
	}
	defer unlock()
	r, ok := c.t.contacts.get(id, false)
	if !ok {
		return crm.Contact{}, notFound("contact", id)
	}
	return cloneContact(r.v), nil
}

func (c *client) UpdateContact(ctx context.Context, id crm.ID, p crm.ContactPatch, ifVersion int64) (crm.Contact, error) {
	if err := ctx.Err(); err != nil {
		return crm.Contact{}, err
	}
	p, err := crm.NormalizeContactPatch(p)
	if err != nil {
		return crm.Contact{}, err
	}
	unlock, err := c.begin(ctx)
	if err != nil {
		return crm.Contact{}, err
	}
	defer unlock()
	r, ok := c.t.contacts.get(id, false)
	if !ok {
		return crm.Contact{}, notFound("contact", id)
	}
	if err := c.checkVersion(ifVersion, r.v.Version); err != nil {
		return crm.Contact{}, err
	}
	next := cloneContact(r.v)
	set(&next.FirstName, p.FirstName)
	set(&next.LastName, p.LastName)
	set(&next.Email, p.Email)
	set(&next.Phone, p.Phone)
	set(&next.Title, p.Title)
	set(&next.OwnerSub, p.OwnerSub)
	if next.FirstName == "" && next.LastName == "" && next.Email == "" {
		return crm.Contact{}, fmt.Errorf("%w: a contact needs a name or an email", crm.ErrInvalid)
	}
	if next.Fields, err = crm.MergeFields(next.Fields, p.Fields); err != nil {
		return crm.Contact{}, err
	}
	c.bump(&next.Version, &next.UpdatedAt)
	r.v = next
	return cloneContact(next), nil
}

func (c *client) ArchiveContact(ctx context.Context, id crm.ID, ifVersion int64) error {
	unlock, err := c.begin(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	r, ok := c.t.contacts.get(id, false)
	if !ok {
		return notFound("contact", id)
	}
	if err := c.checkVersion(ifVersion, r.v.Version); err != nil {
		return err
	}
	r.archived = true
	c.bump(&r.v.Version, &r.v.UpdatedAt)
	return nil
}

func (c *client) ListContacts(ctx context.Context, f crm.ContactFilter, p crm.PageRequest) (crm.Page[crm.Contact], error) {
	unlock, err := c.begin(ctx)
	if err != nil {
		return crm.Page[crm.Contact]{}, err
	}
	defer unlock()
	return list(c.t.contacts, f.IncludeArchived, func(x crm.Contact) bool {
		return (contains(x.FirstName+" "+x.LastName, f.Query) || contains(x.Email, f.Query)) &&
			(f.CompanyID == "" || x.CompanyID == f.CompanyID) && (f.OwnerSub == "" || x.OwnerSub == f.OwnerSub)
	}, cloneContact, p)
}

// ───────────────────────────── deals ─────────────────────────────

func (c *client) CreateDeal(ctx context.Context, key string, in crm.DealInput) (crm.Deal, bool, error) {
	if err := ctx.Err(); err != nil {
		return crm.Deal{}, false, err
	}
	if err := crm.ValidateKey(key); err != nil {
		return crm.Deal{}, false, err
	}
	unlock, err := c.begin(ctx)
	if err != nil {
		return crm.Deal{}, false, err
	}
	defer unlock()
	in, _, err = crm.NormalizeDeal(in, c.t.pipelines)
	if err != nil {
		return crm.Deal{}, false, err
	}
	hash := crm.HashInput(in)
	if id, found, err := c.t.replay("deal", key, hash); err != nil {
		return crm.Deal{}, false, err
	} else if found {
		r, _ := c.t.deals.get(id, true)
		return cloneDeal(r.v), false, nil
	}
	if err := c.requireCompany(in.CompanyID, true); err != nil {
		return crm.Deal{}, false, err
	}
	if err := c.requireContact(in.ContactID); err != nil {
		return crm.Deal{}, false, err
	}
	ts := now()
	d := crm.Deal{
		ID: crm.ID(uuid.NewString()), Name: in.Name, CompanyID: in.CompanyID, ContactID: in.ContactID,
		Pipeline: in.Pipeline, Stage: in.Stage, Amount: in.Amount, ExpectedClose: in.ExpectedClose,
		OwnerSub: in.OwnerSub, Fields: in.Fields, StageChangedAt: ts,
		Version: c.firstVersion(), CreatedAt: ts, UpdatedAt: ts,
	}
	c.t.deals.insert(c.t.nextSeq(), d.ID, d)
	c.t.history[d.ID] = []crm.StageChange{{To: d.Stage, ChangedBy: in.CreatedBy, ChangedAt: ts}}
	c.t.remember("deal", key, hash, d.ID)
	return cloneDeal(d), true, nil
}

func (c *client) GetDeal(ctx context.Context, id crm.ID) (crm.Deal, error) {
	unlock, err := c.begin(ctx)
	if err != nil {
		return crm.Deal{}, err
	}
	defer unlock()
	r, ok := c.t.deals.get(id, false)
	if !ok {
		return crm.Deal{}, notFound("deal", id)
	}
	return cloneDeal(r.v), nil
}

func (c *client) UpdateDeal(ctx context.Context, id crm.ID, p crm.DealPatch, ifVersion int64) (crm.Deal, error) {
	if err := ctx.Err(); err != nil {
		return crm.Deal{}, err
	}
	unlock, err := c.begin(ctx)
	if err != nil {
		return crm.Deal{}, err
	}
	defer unlock()
	r, ok := c.t.deals.get(id, false)
	if !ok {
		return crm.Deal{}, notFound("deal", id)
	}
	pipe, _ := crm.FindPipeline(c.t.pipelines, r.v.Pipeline)
	if p, err = crm.NormalizeDealPatch(p, pipe); err != nil {
		return crm.Deal{}, err
	}
	if err := c.checkVersion(ifVersion, r.v.Version); err != nil {
		return crm.Deal{}, err
	}
	next := cloneDeal(r.v)
	set(&next.Name, p.Name)
	set(&next.OwnerSub, p.OwnerSub)
	if p.ContactID != nil {
		if err := c.requireContact(*p.ContactID); err != nil {
			return crm.Deal{}, err
		}
		next.ContactID = *p.ContactID
	}
	switch {
	case p.ClearAmount:
		next.Amount = nil
	case p.Amount != nil:
		a := *p.Amount
		next.Amount = &a
	}
	switch {
	case p.ClearExpectedClose:
		next.ExpectedClose = time.Time{}
	case p.ExpectedClose != nil:
		next.ExpectedClose = *p.ExpectedClose
	}
	if next.Fields, err = crm.MergeFields(next.Fields, p.Fields); err != nil {
		return crm.Deal{}, err
	}
	c.bump(&next.Version, &next.UpdatedAt)
	r.v = next
	return cloneDeal(next), nil
}

func (c *client) MoveDeal(ctx context.Context, id crm.ID, toStage, by string, ifVersion int64) (crm.Deal, error) {
	unlock, err := c.begin(ctx)
	if err != nil {
		return crm.Deal{}, err
	}
	defer unlock()
	r, ok := c.t.deals.get(id, false)
	if !ok {
		return crm.Deal{}, notFound("deal", id)
	}
	pipe, _ := crm.FindPipeline(c.t.pipelines, r.v.Pipeline)
	if !crm.HasStage(pipe, toStage) {
		return crm.Deal{}, fmt.Errorf("%w: pipeline %q has no stage %q", crm.ErrInvalid, pipe.Key, toStage)
	}
	if utf8.RuneCountInString(by) > 200 {
		return crm.Deal{}, fmt.Errorf("%w: changed-by is longer than 200 characters", crm.ErrInvalid)
	}
	if err := c.checkVersion(ifVersion, r.v.Version); err != nil {
		return crm.Deal{}, err
	}
	if r.v.Stage == toStage {
		return cloneDeal(r.v), nil // no-op: no version bump, no history entry
	}
	from := r.v.Stage
	r.v.Stage = toStage
	r.v.StageChangedAt = now()
	c.bump(&r.v.Version, &r.v.UpdatedAt)
	r.v.UpdatedAt = r.v.StageChangedAt
	c.t.history[id] = append(c.t.history[id], crm.StageChange{From: from, To: toStage, ChangedBy: by, ChangedAt: r.v.StageChangedAt})
	return cloneDeal(r.v), nil
}

func (c *client) ArchiveDeal(ctx context.Context, id crm.ID, ifVersion int64) error {
	unlock, err := c.begin(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	r, ok := c.t.deals.get(id, false)
	if !ok {
		return notFound("deal", id)
	}
	if err := c.checkVersion(ifVersion, r.v.Version); err != nil {
		return err
	}
	r.archived = true
	c.bump(&r.v.Version, &r.v.UpdatedAt)
	return nil
}

func (c *client) ListDeals(ctx context.Context, f crm.DealFilter, p crm.PageRequest) (crm.Page[crm.Deal], error) {
	unlock, err := c.begin(ctx)
	if err != nil {
		return crm.Page[crm.Deal]{}, err
	}
	defer unlock()
	return list(c.t.deals, f.IncludeArchived, func(x crm.Deal) bool {
		return contains(x.Name, f.Query) && (f.Pipeline == "" || x.Pipeline == f.Pipeline) && (f.Stage == "" || x.Stage == f.Stage) &&
			(f.CompanyID == "" || x.CompanyID == f.CompanyID) && (f.OwnerSub == "" || x.OwnerSub == f.OwnerSub)
	}, cloneDeal, p)
}

func (c *client) DealHistory(ctx context.Context, id crm.ID) ([]crm.StageChange, error) {
	unlock, err := c.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if c.s.opts.NoStageHistory {
		return nil, fmt.Errorf("%w: stage history", crm.ErrUnsupported)
	}
	if _, ok := c.t.deals.get(id, false); !ok {
		return nil, notFound("deal", id)
	}
	return slices.Clone(c.t.history[id]), nil
}

// ───────────────────────────── activities ─────────────────────────────

func (c *client) CreateActivity(ctx context.Context, key string, in crm.ActivityInput) (crm.Activity, bool, error) {
	if err := ctx.Err(); err != nil {
		return crm.Activity{}, false, err
	}
	if err := crm.ValidateKey(key); err != nil {
		return crm.Activity{}, false, err
	}
	in, err := crm.NormalizeActivity(in)
	if err != nil {
		return crm.Activity{}, false, err
	}
	unlock, err := c.begin(ctx)
	if err != nil {
		return crm.Activity{}, false, err
	}
	defer unlock()
	hash := crm.HashInput(in)
	if id, found, err := c.t.replay("activity", key, hash); err != nil {
		return crm.Activity{}, false, err
	} else if found {
		r, _ := c.t.acts.get(id, true)
		return r.v, false, nil
	}
	if err := c.requireCompany(in.CompanyID, false); err != nil {
		return crm.Activity{}, false, err
	}
	if err := c.requireContact(in.ContactID); err != nil {
		return crm.Activity{}, false, err
	}
	if in.DealID != "" {
		if _, ok := c.t.deals.get(in.DealID, false); !ok {
			return crm.Activity{}, false, fmt.Errorf("%w: deal %q does not exist", crm.ErrInvalid, in.DealID)
		}
	}
	a := crm.Activity{
		ID: crm.ID(uuid.NewString()), Kind: in.Kind, Subject: in.Subject, Body: in.Body, DueAt: in.DueAt,
		CompanyID: in.CompanyID, ContactID: in.ContactID, DealID: in.DealID, OwnerSub: in.OwnerSub, CreatedBy: in.CreatedBy,
		CreatedAt: now(),
	}
	c.t.acts.insert(c.t.nextSeq(), a.ID, a)
	c.t.remember("activity", key, hash, a.ID)
	return a, true, nil
}

func (c *client) GetActivity(ctx context.Context, id crm.ID) (crm.Activity, error) {
	unlock, err := c.begin(ctx)
	if err != nil {
		return crm.Activity{}, err
	}
	defer unlock()
	r, ok := c.t.acts.get(id, false)
	if !ok {
		return crm.Activity{}, notFound("activity", id)
	}
	return r.v, nil
}

func (c *client) CompleteActivity(ctx context.Context, id crm.ID, by string) (crm.Activity, error) {
	unlock, err := c.begin(ctx)
	if err != nil {
		return crm.Activity{}, err
	}
	defer unlock()
	r, ok := c.t.acts.get(id, false)
	if !ok {
		return crm.Activity{}, notFound("activity", id)
	}
	if r.v.Kind != crm.ActivityTask {
		return crm.Activity{}, fmt.Errorf("%w: only a TASK can be completed, this is a %s", crm.ErrInvalid, r.v.Kind)
	}
	if r.v.CompletedAt.IsZero() {
		r.v.CompletedAt = now()
	}
	return r.v, nil
}

func (c *client) ListActivities(ctx context.Context, f crm.ActivityFilter, p crm.PageRequest) (crm.Page[crm.Activity], error) {
	unlock, err := c.begin(ctx)
	if err != nil {
		return crm.Page[crm.Activity]{}, err
	}
	defer unlock()
	return list(c.t.acts, true, func(x crm.Activity) bool {
		return (f.CompanyID == "" || x.CompanyID == f.CompanyID) && (f.ContactID == "" || x.ContactID == f.ContactID) &&
			(f.DealID == "" || x.DealID == f.DealID) && (f.Kind == "" || x.Kind == f.Kind) &&
			(f.OwnerSub == "" || x.OwnerSub == f.OwnerSub) &&
			(!f.OpenTasksOnly || (x.Kind == crm.ActivityTask && x.CompletedAt.IsZero()))
	}, func(a crm.Activity) crm.Activity { return a }, p)
}

// ───────────────────────────── references ─────────────────────────────

// requireCompany checks a parent reference. An empty id is fine unless the parent is mandatory. A reference to a
// record that does not exist, is archived, or belongs to another tenant is ErrInvalid.
func (c *client) requireCompany(id crm.ID, mandatory bool) error {
	if id == "" {
		if mandatory {
			return fmt.Errorf("%w: a company is required", crm.ErrInvalid)
		}
		return nil
	}
	if _, ok := c.t.companies.get(id, false); !ok {
		return fmt.Errorf("%w: company %q does not exist", crm.ErrInvalid, id)
	}
	return nil
}

func (c *client) requireContact(id crm.ID) error {
	if id == "" {
		return nil
	}
	if _, ok := c.t.contacts.get(id, false); !ok {
		return fmt.Errorf("%w: contact %q does not exist", crm.ErrInvalid, id)
	}
	return nil
}

// ───────────────────────────── helpers ─────────────────────────────

func notFound(kind string, id crm.ID) error {
	return fmt.Errorf("%w: %s %q", crm.ErrNotFound, kind, id)
}

func set(dst *string, v *string) {
	if v != nil {
		*dst = *v
	}
}

// contains is a case-insensitive substring test; an empty needle matches everything. Wildcards are literal.
func contains(hay, needle string) bool {
	needle = strings.TrimSpace(needle)
	return needle == "" || strings.Contains(strings.ToLower(hay), strings.ToLower(needle))
}

// list returns one page, oldest-created first. The cursor is the sequence number of the last item returned, so a
// record created while someone pages is simply seen later and never shifts existing records between pages.
func list[T any](t *table[T], includeArchived bool, match func(T) bool, clone func(T) T, req crm.PageRequest) (crm.Page[T], error) {
	size, err := crm.PageSize(req)
	if err != nil {
		return crm.Page[T]{}, err
	}
	after, err := crm.ParseSeqCursor(req.Cursor)
	if err != nil {
		return crm.Page[T]{}, err
	}
	var out crm.Page[T]
	var last int64
	for _, r := range t.order {
		if r.seq <= after || (r.archived && !includeArchived) || !match(r.v) {
			continue
		}
		if len(out.Items) == size {
			out.Next = crm.SeqCursor(last)
			break
		}
		out.Items = append(out.Items, clone(r.v))
		last = r.seq
	}
	return out, nil
}

func cloneFields(f map[string]string) map[string]string {
	out := make(map[string]string, len(f))
	for k, v := range f {
		out[k] = v
	}
	return out
}

func cloneCompany(c crm.Company) crm.Company { c.Fields = cloneFields(c.Fields); return c }
func cloneContact(c crm.Contact) crm.Contact { c.Fields = cloneFields(c.Fields); return c }
func cloneDeal(d crm.Deal) crm.Deal {
	d.Fields = cloneFields(d.Fields)
	if d.Amount != nil {
		a := *d.Amount
		d.Amount = &a
	}
	return d
}

func clonePipelines(ps []crm.Pipeline) []crm.Pipeline {
	out := make([]crm.Pipeline, len(ps))
	for i, p := range ps {
		p.Stages = slices.Clone(p.Stages)
		p.CustomFields = slices.Clone(p.CustomFields)
		out[i] = p
	}
	return out
}

// Package provisioner turns an approved Blueprint into a live tenant across Postgres, Keycloak, ERPNext, Twenty,
// LibreDesk and Hermes.
//
// It is a deterministic saga, not an agent: a fixed list of steps grouped into parallel stages, each step
// idempotent (the ports' contract), persisted before and after it runs, retried on transient errors, and undone in
// reverse order if anything fails. Replaying the same idempotency key returns the stored outcome (or resumes a run
// that crashed mid-way) and never provisions twice.
package provisioner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/ports"
	"github.com/mrbiz/venture-os/internal/skillgen"
)

// Defaults.
const (
	DefaultTimeout       = 60 * time.Second
	DefaultTokenTTL      = 30 * 24 * time.Hour
	DefaultLease         = 2 * time.Minute
	compensationTimeout  = 45 * time.Second
	defaultRetryAttempts = 4
)

type RunStatus string

const (
	RunPending            RunStatus = "PENDING"
	RunRunning            RunStatus = "RUNNING"
	RunSucceeded          RunStatus = "SUCCEEDED"
	RunFailed             RunStatus = "FAILED"
	RunCompensating       RunStatus = "COMPENSATING"
	RunRolledBack         RunStatus = "ROLLED_BACK"
	RunCompensationFailed RunStatus = "COMPENSATION_FAILED"
)

type StepStatus string

const (
	StepPending            StepStatus = "PENDING"
	StepRunning            StepStatus = "RUNNING"
	StepSucceeded          StepStatus = "SUCCEEDED"
	StepFailed             StepStatus = "FAILED"
	StepCompensated        StepStatus = "COMPENSATED"
	StepCompensationFailed StepStatus = "COMPENSATION_FAILED"
)

// Run is the persisted header of one provisioning attempt.
type Run struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	BlueprintID uuid.UUID
	Status      RunStatus
	Error       string
}

// StepRecord is the persisted state of one step.
type StepRecord struct {
	Key      string
	Ordinal  int
	Status   StepStatus
	Attempts int
	Ref      ports.Ref
	Error    string
}

// Store persists saga state and seeds the tenant's own Postgres rows. All methods are tenant-scoped and
// idempotent; the Postgres implementation lives in internal/store.
type Store interface {
	// BeginRun finds or creates the run for (tenant, key). claimed reports whether the caller now owns it and must
	// execute it. A terminal run is returned with claimed=false; a live run younger than lease is returned with
	// claimed=false (someone else is on it); a stale live run is taken over (claimed=true).
	BeginRun(ctx context.Context, tenant, blueprint uuid.UUID, key string, lease time.Duration) (run Run, claimed bool, err error)
	Steps(ctx context.Context, tenant, run uuid.UUID) ([]StepRecord, error)
	SaveStep(ctx context.Context, tenant, run uuid.UUID, s StepRecord) error
	FinishRun(ctx context.Context, tenant, run uuid.UUID, status RunStatus, errMsg string) error
	// SetRunStatus records a non-terminal transition (RUNNING, COMPENSATING) and refreshes the lease.
	SetRunStatus(ctx context.Context, tenant, run uuid.UUID, status RunStatus) error

	SeedTenant(ctx context.Context, tenant, blueprint uuid.UUID, bp domain.Blueprint, skills []ports.Skill) error
	RetireSeed(ctx context.Context, tenant uuid.UUID) error
	SetTenantStatus(ctx context.Context, tenant uuid.UUID, status string) error
	RecordIntegrations(ctx context.Context, tenant uuid.UUID, refs map[string]ports.Ref) error
	Audit(ctx context.Context, tenant uuid.UUID, action string, data map[string]any) error
}

// TokenMinter issues the MCP bearer token a tenant's Hermes profile uses (see internal/mcptoken).
type TokenMinter interface {
	Mint(tenant uuid.UUID, agent string, ttl time.Duration) (string, error)
}

// Retry controls retries of transient (ports.ErrTransient) failures.
type Retry struct {
	Attempts int
	Base     time.Duration
	Max      time.Duration
}

// Provisioner holds the dependencies of the saga.
type Provisioner struct {
	Identity ports.IdentityProvider
	ERP      ports.ERP
	CRM      ports.CRM
	Support  ports.Support
	Runtime  ports.AgentRuntime
	Store    Store
	Tokens   TokenMinter

	Timeout  time.Duration // forward-phase budget; default 60s
	TokenTTL time.Duration
	Lease    time.Duration
	Retry    Retry
	// Sleep waits for d or until ctx ends. Defaults to a real timer; tests inject a no-op.
	Sleep func(ctx context.Context, d time.Duration) error
	// Log receives non-fatal problems (e.g. a failed audit write). Defaults to slog.Default().
	Log *slog.Logger
}

// Request is one provisioning command.
type Request struct {
	Tenant         ports.TenantInfo
	FounderSub     string
	BlueprintID    uuid.UUID
	Blueprint      domain.Blueprint
	IdempotencyKey string
}

// Result is the outcome of a successful (or replayed) run.
type Result struct {
	RunID    uuid.UUID
	Status   RunStatus
	Steps    []StepRecord
	Refs     map[string]ports.Ref
	Replayed bool
	Elapsed  time.Duration
}

var (
	// ErrInProgress means another worker holds a live run for this idempotency key.
	ErrInProgress = errors.New("provisioning already in progress for this idempotency key")
	// ErrInvalidRequest means the request itself is unusable; nothing was touched.
	ErrInvalidRequest = errors.New("invalid provisioning request")
)

// Error describes a failed run.
type Error struct {
	RunID      uuid.UUID
	Step       string
	Cause      error
	Status     RunStatus // ROLLED_BACK or COMPENSATION_FAILED
	CleanupErr error
}

func (e *Error) Error() string {
	s := fmt.Sprintf("provisioning failed at step %q: %v (run %s, %s)", e.Step, e.Cause, e.RunID, e.Status)
	if e.CleanupErr != nil {
		s += fmt.Sprintf("; cleanup incomplete: %v", e.CleanupErr)
	}
	return s
}
func (e *Error) Unwrap() error { return e.Cause }

// ───────────────────────────── steps ─────────────────────────────

type runState struct {
	req    Request
	skills []ports.Skill
	token  string
	mu     sync.Mutex
	refs   map[string]ports.Ref
}

func (s *runState) get(key string) ports.Ref {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refs[key]
}
func (s *runState) set(key string, r ports.Ref) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs[key] = r
}

type step struct {
	key   string
	stage int
	do    func(ctx context.Context, p *Provisioner, s *runState) (ports.Ref, error)
	undo  func(ctx context.Context, p *Provisioner, s *runState, ref ports.Ref) error // nil: covered by an earlier step's undo
}

// Step keys double as keys of Result.Refs; the *system* refs (identity, erp, ...) are what get recorded in
// tenant_integrations.
const (
	stIdentity  = "identity.organization"
	stSeed      = "db.seed"
	stERP       = "erp.company"
	stCRM       = "crm.workspace"
	stSupport   = "support.inboxes"
	stRuntime   = "runtime.profile"
	stPipelines = "crm.pipelines"
	stKB        = "support.kb"
	stSkills    = "runtime.skills"
	stActivate  = "tenant.activate"
)

func tenantInfo(s *runState) ports.TenantInfo { return s.req.Tenant }

func steps() []step {
	return []step{
		// Stage 1: foundations.
		{key: stIdentity, stage: 1,
			do: func(ctx context.Context, p *Provisioner, s *runState) (ports.Ref, error) {
				return p.Identity.EnsureOrganization(ctx, tenantInfo(s), s.req.FounderSub)
			},
			undo: func(ctx context.Context, p *Provisioner, s *runState, r ports.Ref) error {
				return p.Identity.RemoveOrganization(ctx, tenantInfo(s), r)
			}},
		{key: stSeed, stage: 1,
			do: func(ctx context.Context, p *Provisioner, s *runState) (ports.Ref, error) {
				return "", p.Store.SeedTenant(ctx, s.req.Tenant.ID, s.req.BlueprintID, s.req.Blueprint, s.skills)
			},
			undo: func(ctx context.Context, p *Provisioner, s *runState, _ ports.Ref) error {
				return p.Store.RetireSeed(ctx, s.req.Tenant.ID)
			}},
		// Stage 2: independent systems, created in parallel.
		{key: stERP, stage: 2,
			do: func(ctx context.Context, p *Provisioner, s *runState) (ports.Ref, error) {
				return p.ERP.EnsureCompany(ctx, tenantInfo(s), s.req.Blueprint.Business, s.req.Blueprint.Accounting)
			},
			undo: func(ctx context.Context, p *Provisioner, s *runState, r ports.Ref) error {
				return p.ERP.RemoveCompany(ctx, tenantInfo(s), r)
			}},
		{key: stCRM, stage: 2,
			do: func(ctx context.Context, p *Provisioner, s *runState) (ports.Ref, error) {
				return p.CRM.EnsureWorkspace(ctx, tenantInfo(s))
			},
			undo: func(ctx context.Context, p *Provisioner, s *runState, r ports.Ref) error {
				return p.CRM.RemoveWorkspace(ctx, tenantInfo(s), r)
			}},
		{key: stSupport, stage: 2,
			do: func(ctx context.Context, p *Provisioner, s *runState) (ports.Ref, error) {
				return p.Support.EnsureInboxes(ctx, tenantInfo(s), s.req.Blueprint.Support.Channels)
			},
			undo: func(ctx context.Context, p *Provisioner, s *runState, r ports.Ref) error {
				return p.Support.RemoveInboxes(ctx, tenantInfo(s), r)
			}},
		{key: stRuntime, stage: 2,
			do: func(ctx context.Context, p *Provisioner, s *runState) (ports.Ref, error) {
				return p.Runtime.EnsureProfile(ctx, tenantInfo(s), s.token)
			},
			undo: func(ctx context.Context, p *Provisioner, s *runState, r ports.Ref) error {
				return p.Runtime.RemoveProfile(ctx, tenantInfo(s), r)
			}},
		// Stage 3: configuration that needs a stage-2 system to exist. Removing the parent removes these.
		{key: stPipelines, stage: 3,
			do: func(ctx context.Context, p *Provisioner, s *runState) (ports.Ref, error) {
				return "", p.CRM.ApplyPipelines(ctx, tenantInfo(s), s.get(stCRM), s.req.Blueprint.CRM.Pipelines)
			}},
		{key: stKB, stage: 3,
			do: func(ctx context.Context, p *Provisioner, s *runState) (ports.Ref, error) {
				return "", p.Support.SeedKnowledgeBase(ctx, tenantInfo(s), s.get(stSupport), s.req.Blueprint.Support.KBDocs)
			}},
		{key: stSkills, stage: 3,
			do: func(ctx context.Context, p *Provisioner, s *runState) (ports.Ref, error) {
				return "", p.Runtime.InstallSkills(ctx, tenantInfo(s), s.get(stRuntime), s.skills)
			}},
		// Stage 4: go live. Only now can the MCP bus and UI treat the tenant as active.
		{key: stActivate, stage: 4,
			do: func(ctx context.Context, p *Provisioner, s *runState) (ports.Ref, error) {
				refs := map[string]ports.Ref{}
				for sys, key := range map[string]string{
					"keycloak": stIdentity, "erpnext": stERP, "twenty": stCRM,
					"libredesk": stSupport, "hermes": stRuntime,
				} {
					refs[sys] = s.get(key)
				}
				if err := p.Store.RecordIntegrations(ctx, s.req.Tenant.ID, refs); err != nil {
					return "", err
				}
				return "", p.Store.SetTenantStatus(ctx, s.req.Tenant.ID, "ACTIVE")
			},
			undo: func(ctx context.Context, p *Provisioner, s *runState, _ ports.Ref) error {
				return p.Store.SetTenantStatus(ctx, s.req.Tenant.ID, "DRAFT")
			}},
	}
}

// ───────────────────────────── entry point ─────────────────────────────

// Provision runs (or replays) the saga for req.
func (p *Provisioner) Provision(ctx context.Context, req Request) (Result, error) {
	start := time.Now()
	if err := p.validate(req); err != nil {
		return Result{}, err
	}
	// Render and lint every skill, and mint the profile token, BEFORE touching any external system.
	skills, err := skillgen.RenderAll(req.Blueprint)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	token, err := p.Tokens.Mint(req.Tenant.ID, "", p.tokenTTL())
	if err != nil {
		return Result{}, fmt.Errorf("provisioner: mint mcp token: %w", err)
	}

	run, claimed, err := p.Store.BeginRun(ctx, req.Tenant.ID, req.BlueprintID, req.IdempotencyKey, p.lease())
	if err != nil {
		return Result{}, fmt.Errorf("provisioner: begin run: %w", err)
	}
	// An idempotency key names ONE command. BeginRun finds an earlier run by key alone, so without this check a key reused
	// for a different blueprint would be answered with the old run's outcome (a "replayed success" for a blueprint that
	// was never provisioned, or the old failure), or would resume the old run's steps under the new blueprint.
	if run.BlueprintID != uuid.Nil && run.BlueprintID != req.BlueprintID {
		return Result{}, fmt.Errorf("%w: idempotency key %q was already used to provision a different blueprint; use a new key", ErrInvalidRequest, req.IdempotencyKey)
	}
	if !claimed {
		return p.replay(ctx, req, run)
	}

	st := &runState{req: req, skills: skills, token: token, refs: map[string]ports.Ref{}}
	fwdCtx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()

	p.audit(ctx, req.Tenant.ID, "provision.start", map[string]any{"run_id": run.ID, "blueprint_id": req.BlueprintID})
	if err := p.Store.SetTenantStatus(fwdCtx, req.Tenant.ID, "PROVISIONING"); err != nil {
		return Result{}, p.abort(ctx, req, run, st, "tenant.begin", err)
	}
	if err := p.Store.SetRunStatus(fwdCtx, req.Tenant.ID, run.ID, RunRunning); err != nil {
		return Result{}, p.abort(ctx, req, run, st, "run.begin", err)
	}

	prior, err := p.Store.Steps(fwdCtx, req.Tenant.ID, run.ID)
	if err != nil {
		return Result{}, p.abort(ctx, req, run, st, "run.load", err)
	}
	done := map[string]StepRecord{}
	for _, r := range prior {
		done[r.Key] = r
	}

	all := steps()
	if failedStep, err := p.runStages(fwdCtx, req, run, st, all, done); err != nil {
		return Result{}, p.abort(ctx, req, run, st, failedStep, err)
	}

	if err := p.Store.FinishRun(ctx, req.Tenant.ID, run.ID, RunSucceeded, ""); err != nil {
		return Result{}, fmt.Errorf("provisioner: finish run: %w", err)
	}
	p.audit(ctx, req.Tenant.ID, "provision.succeeded", map[string]any{"run_id": run.ID, "elapsed_ms": time.Since(start).Milliseconds()})
	recs, _ := p.Store.Steps(ctx, req.Tenant.ID, run.ID)
	return Result{RunID: run.ID, Status: RunSucceeded, Steps: recs, Refs: st.snapshot(), Elapsed: time.Since(start)}, nil
}

func (p *Provisioner) validate(req Request) error {
	switch {
	case p.Store == nil || p.Tokens == nil || p.Identity == nil || p.ERP == nil || p.CRM == nil ||
		p.Support == nil || p.Runtime == nil:
		return errors.New("provisioner: not fully wired")
	case req.Tenant.ID == uuid.Nil:
		return fmt.Errorf("%w: tenant id required", ErrInvalidRequest)
	case req.BlueprintID == uuid.Nil:
		return fmt.Errorf("%w: blueprint id required", ErrInvalidRequest)
	case req.IdempotencyKey == "":
		return fmt.Errorf("%w: idempotency key required", ErrInvalidRequest)
	case req.FounderSub == "":
		return fmt.Errorf("%w: founder subject required", ErrInvalidRequest)
	case req.Tenant.Slug != req.Blueprint.Business.Slug:
		return fmt.Errorf("%w: tenant slug %q does not match blueprint slug %q", ErrInvalidRequest, req.Tenant.Slug, req.Blueprint.Business.Slug)
	}
	if err := req.Blueprint.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	return nil
}

// replay answers a repeated command from the stored run without touching any external system.
func (p *Provisioner) replay(ctx context.Context, req Request, run Run) (Result, error) {
	recs, err := p.Store.Steps(ctx, req.Tenant.ID, run.ID)
	if err != nil {
		return Result{}, err
	}
	switch run.Status {
	case RunSucceeded:
		refs := map[string]ports.Ref{}
		for _, r := range recs {
			if r.Ref != "" {
				refs[r.Key] = r.Ref
			}
		}
		return Result{RunID: run.ID, Status: run.Status, Steps: recs, Refs: refs, Replayed: true}, nil
	case RunFailed, RunRolledBack, RunCompensationFailed:
		failed := ""
		for _, r := range recs {
			if r.Status == StepFailed {
				failed = r.Key
				break
			}
		}
		return Result{}, &Error{RunID: run.ID, Step: failed, Cause: errors.New(run.Error), Status: run.Status}
	default:
		return Result{}, ErrInProgress
	}
}

func (s *runState) snapshot() map[string]ports.Ref {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]ports.Ref, len(s.refs))
	for k, v := range s.refs {
		out[k] = v
	}
	return out
}

func (p *Provisioner) runStages(ctx context.Context, req Request, run Run, st *runState, all []step, done map[string]StepRecord) (string, error) {
	maxStage := 0
	for _, s := range all {
		if s.stage > maxStage {
			maxStage = s.stage
		}
	}
	ordinal := map[string]int{}
	for i, s := range all {
		ordinal[s.key] = i
	}
	for stage := 1; stage <= maxStage; stage++ {
		var group []step
		for _, s := range all {
			if s.stage == stage {
				group = append(group, s)
			}
		}
		stageCtx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		var mu sync.Mutex
		var firstErr error
		firstKey := ""
		for _, s := range group {
			wg.Add(1)
			go func(s step) {
				defer wg.Done()
				if err := p.runStep(stageCtx, req, run, st, s, ordinal[s.key], done); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr, firstKey = err, s.key
						cancel() // stop siblings early; they are compensated below
					}
					mu.Unlock()
				}
			}(s)
		}
		wg.Wait()
		cancel()
		if firstErr != nil {
			return firstKey, firstErr
		}
	}
	return "", nil
}

func (p *Provisioner) runStep(ctx context.Context, req Request, run Run, st *runState, s step, ordinal int, done map[string]StepRecord) error {
	if prev, ok := done[s.key]; ok && prev.Status == StepSucceeded {
		st.set(s.key, prev.Ref) // resumed run: reuse what an earlier attempt already created
		return nil
	}
	rec := StepRecord{Key: s.key, Ordinal: ordinal, Status: StepRunning}
	if err := p.Store.SaveStep(ctx, req.Tenant.ID, run.ID, rec); err != nil {
		return err
	}
	var ref ports.Ref
	attempts, err := p.withRetry(ctx, func() error {
		var e error
		ref, e = s.do(ctx, p, st)
		return e
	})
	rec.Attempts = attempts
	if err != nil {
		rec.Status, rec.Error = StepFailed, err.Error()
		// Persist with a context that survives the cancellation that may have caused the failure.
		_ = p.Store.SaveStep(context.WithoutCancel(ctx), req.Tenant.ID, run.ID, rec)
		return err
	}
	rec.Status, rec.Ref = StepSucceeded, ref
	st.set(s.key, ref)
	return p.Store.SaveStep(ctx, req.Tenant.ID, run.ID, rec)
}

// abort compensates everything that ran and returns the *Error to report.
func (p *Provisioner) abort(ctx context.Context, req Request, run Run, st *runState, failedStep string, cause error) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), compensationTimeout)
	defer cancel()
	_ = p.Store.SetRunStatus(cctx, req.Tenant.ID, run.ID, RunCompensating)

	recs, _ := p.Store.Steps(cctx, req.Tenant.ID, run.ID)
	byKey := map[string]StepRecord{}
	for _, r := range recs {
		byKey[r.Key] = r
	}
	all := steps()
	var cleanupErrs []error
	// Reverse order. FAILED/RUNNING steps are undone too: an Ensure that errored may still have created something.
	for i := len(all) - 1; i >= 0; i-- {
		s := all[i]
		rec, touched := byKey[s.key]
		if !touched || s.undo == nil || rec.Status == StepCompensated {
			continue
		}
		ref := rec.Ref
		if ref == "" {
			ref = st.get(s.key)
		}
		_, err := p.withRetry(cctx, func() error { return s.undo(cctx, p, st, ref) })
		if err != nil {
			rec.Status, rec.Error = StepCompensationFailed, err.Error()
			cleanupErrs = append(cleanupErrs, fmt.Errorf("%s: %w", s.key, err))
		} else {
			rec.Status = StepCompensated
		}
		_ = p.Store.SaveStep(cctx, req.Tenant.ID, run.ID, rec)
	}
	// Never leave the tenant looking half-built.
	if len(cleanupErrs) == 0 {
		_ = p.Store.SetTenantStatus(cctx, req.Tenant.ID, "DRAFT")
	}

	status := RunRolledBack
	var cleanup error
	if len(cleanupErrs) > 0 {
		status, cleanup = RunCompensationFailed, errors.Join(cleanupErrs...)
	}
	_ = p.Store.FinishRun(cctx, req.Tenant.ID, run.ID, status, cause.Error())
	p.audit(cctx, req.Tenant.ID, "provision.failed", map[string]any{
		"run_id": run.ID, "step": failedStep, "error": cause.Error(), "status": string(status),
	})
	return &Error{RunID: run.ID, Step: failedStep, Cause: cause, Status: status, CleanupErr: cleanup}
}

// ───────────────────────────── retry ─────────────────────────────

// withRetry runs fn, retrying only ports.ErrTransient failures with exponential backoff. It returns the number
// of attempts made.
func (p *Provisioner) withRetry(ctx context.Context, fn func() error) (int, error) {
	max := p.Retry.Attempts
	if max <= 0 {
		max = defaultRetryAttempts
	}
	delay := p.Retry.Base
	if delay <= 0 {
		delay = 200 * time.Millisecond
	}
	ceiling := p.Retry.Max
	if ceiling <= 0 {
		ceiling = 3 * time.Second
	}
	var err error
	for attempt := 1; ; attempt++ {
		err = fn()
		if err == nil {
			return attempt, nil
		}
		if !errors.Is(err, ports.ErrTransient) || attempt >= max || ctx.Err() != nil {
			return attempt, err
		}
		if serr := p.sleep(ctx, delay); serr != nil {
			return attempt, err
		}
		if delay *= 2; delay > ceiling {
			delay = ceiling
		}
	}
}

func (p *Provisioner) sleep(ctx context.Context, d time.Duration) error {
	if p.Sleep != nil {
		return p.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// audit writes an audit record. A failed audit write must not undo a provisioning outcome, but it must never be
// silent either.
func (p *Provisioner) audit(ctx context.Context, tenant uuid.UUID, action string, data map[string]any) {
	if err := p.Store.Audit(ctx, tenant, action, data); err != nil {
		log := p.Log
		if log == nil {
			log = slog.Default()
		}
		log.Error("audit write failed", "tenant", tenant, "action", action, "err", err)
	}
}

func (p *Provisioner) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return DefaultTimeout
}
func (p *Provisioner) tokenTTL() time.Duration {
	if p.TokenTTL > 0 {
		return p.TokenTTL
	}
	return DefaultTokenTTL
}
func (p *Provisioner) lease() time.Duration {
	if p.Lease > 0 {
		return p.Lease
	}
	return DefaultLease
}

// StepKeys lists step keys in execution order (for UIs and tests).
func StepKeys() []string {
	all := steps()
	keys := make([]string, len(all))
	for i, s := range all {
		keys[i] = s.key
	}
	return keys
}

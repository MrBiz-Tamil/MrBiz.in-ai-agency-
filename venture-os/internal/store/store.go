// Package store is the PostgreSQL implementation of the control plane's persistence.
//
// Every method runs in its own short transaction scoped to a tenant (see internal/tenancy), so row-level security
// is the final arbiter of what is visible or writable: a bug in a query here cannot leak across tenants.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/ports"
	"github.com/mrbiz/venture-os/internal/provisioner"
	"github.com/mrbiz/venture-os/internal/skillgen"
	"github.com/mrbiz/venture-os/internal/tenancy"
)

var (
	ErrNotFound    = errors.New("not found")
	ErrSlugTaken   = errors.New("that business address (slug) is already taken")
	ErrTenantBusy  = errors.New("tenant already has an active provisioning run")
	ErrNotApproved = errors.New("blueprint is not approved")
	ErrTenantLive  = errors.New("tenant is already active; changing a live business needs a re-provisioning flow")
	ErrConflict    = errors.New("conflicting state")
)

// PG is the Postgres store.
type PG struct {
	pool *pgxpool.Pool
	opts tenancy.Options
}

// New returns a store on pool. assumeRole is empty in production (the pool already connects as venture_app);
// tests connect as a superuser and pass "venture_app" so row-level security is enforced.
func New(pool *pgxpool.Pool, assumeRole string) *PG {
	return &PG{pool: pool, opts: tenancy.Options{AssumeRole: assumeRole}}
}

func (s *PG) tx(ctx context.Context, scope tenancy.Scope, fn func(pgx.Tx) error) error {
	return tenancy.WithScopeTx(ctx, s.pool, scope, s.opts, fn)
}

func (s *PG) tenantTx(ctx context.Context, tenant uuid.UUID, fn func(pgx.Tx) error) error {
	return s.tx(ctx, tenancy.Scope{TenantID: tenant}, fn)
}

func pgCode(err error) (code, constraint string) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code, pe.ConstraintName
	}
	return "", ""
}

// ───────────────────────────── tenants & membership ─────────────────────────────

// Tenant is the control-plane view of an organization.
type Tenant struct {
	ID           uuid.UUID
	Slug, Name   string
	Industry     string
	Country      string
	BaseCurrency string
	Timezone     string
	Status       string
}

func (t Tenant) Info() ports.TenantInfo {
	return ports.TenantInfo{ID: t.ID, Slug: t.Slug, Name: t.Name}
}

// Membership is one tenant a user belongs to.
type Membership struct {
	Tenant Tenant
	Role   string
}

// CreateTenant creates a DRAFT organization and its founder membership in one transaction. It is idempotent per
// (founder, key): replaying returns the original tenant id with created=false. The slug is a throwaway until the
// blueprint is approved.
func (s *PG) CreateTenant(ctx context.Context, founderSub, idempotencyKey string) (id uuid.UUID, created bool, err error) {
	id = uuid.New()
	slug := "draft-" + id.String()[:8]
	err = s.tenantTx(ctx, id, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO organizations (id, slug, name, created_by, idempotency_key) VALUES ($1,$2,'Untitled business',$3,$4)`,
			id, slug, founderSub, idempotencyKey); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO tenant_members (tenant_id, user_sub, role) VALUES ($1,$2,'founder')`, id, founderSub)
		return err
	})
	if err == nil {
		return id, true, nil
	}
	if code, c := pgCode(err); code == "23505" && c == "organizations_created_by_idempotency_key_key" {
		existing, lerr := s.findTenantByKey(ctx, founderSub, idempotencyKey)
		if lerr != nil {
			return uuid.Nil, false, lerr
		}
		return existing, false, nil
	}
	return uuid.Nil, false, err
}

func (s *PG) findTenantByKey(ctx context.Context, founderSub, key string) (uuid.UUID, error) {
	ms, err := s.Memberships(ctx, founderSub)
	if err != nil {
		return uuid.Nil, err
	}
	ids := make([]uuid.UUID, len(ms))
	for i, m := range ms {
		ids[i] = m.Tenant.ID
	}
	var id uuid.UUID
	err = s.tx(ctx, tenancy.Scope{ReadTenantIDs: ids}, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM organizations WHERE created_by=$1 AND idempotency_key=$2`, founderSub, key).Scan(&id)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return id, err
}

// Memberships lists the tenants a user belongs to. The membership table is the authority for access.
func (s *PG) Memberships(ctx context.Context, userSub string) ([]Membership, error) {
	var tenantIDs []uuid.UUID
	roles := map[uuid.UUID]string{}
	err := s.tx(ctx, tenancy.Scope{UserSub: userSub}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id, role FROM tenant_members WHERE user_sub=$1`, userSub)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			var role string
			if err := rows.Scan(&id, &role); err != nil {
				return err
			}
			tenantIDs = append(tenantIDs, id)
			roles[id] = role
		}
		return rows.Err()
	})
	if err != nil || len(tenantIDs) == 0 {
		return nil, err
	}
	var out []Membership
	err = s.tx(ctx, tenancy.Scope{ReadTenantIDs: tenantIDs}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, slug, name, coalesce(industry,''), coalesce(country,''), base_currency, timezone, status
		                            FROM organizations ORDER BY created_at, id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t Tenant
			if err := rows.Scan(&t.ID, &t.Slug, &t.Name, &t.Industry, &t.Country, &t.BaseCurrency, &t.Timezone, &t.Status); err != nil {
				return err
			}
			out = append(out, Membership{Tenant: t, Role: roles[t.ID]})
		}
		return rows.Err()
	})
	return out, err
}

// Tenant loads one tenant.
func (s *PG) Tenant(ctx context.Context, id uuid.UUID) (Tenant, error) {
	var t Tenant
	err := s.tenantTx(ctx, id, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id, slug, name, coalesce(industry,''), coalesce(country,''), base_currency, timezone, status
		                         FROM organizations WHERE id=$1`, id).
			Scan(&t.ID, &t.Slug, &t.Name, &t.Industry, &t.Country, &t.BaseCurrency, &t.Timezone, &t.Status)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	return t, err
}

// ───────────────────────────── genesis sessions ─────────────────────────────

// Message is a stored conversation turn.
type Message struct {
	Role    string
	Content string
}

// StartGenesisSession creates (or, replaying the key, returns) the interview session of a tenant.
func (s *PG) StartGenesisSession(ctx context.Context, tenant uuid.UUID, createdBy, key string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO genesis_sessions (tenant_id, created_by, idempotency_key) VALUES ($1,$2,$3)
			 ON CONFLICT (tenant_id, idempotency_key) DO UPDATE SET updated_at = genesis_sessions.updated_at
			 RETURNING id`, tenant, createdBy, key).Scan(&id)
	})
	return id, err
}

// GenesisState returns a session's transcript and last validated draft (nil if none).
func (s *PG) GenesisState(ctx context.Context, tenant, session uuid.UUID) (msgs []Message, draft *domain.Blueprint, status string, err error) {
	err = s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT status, draft_blueprint FROM genesis_sessions WHERE tenant_id=$1 AND id=$2`,
			tenant, session).Scan(&status, &raw); err != nil {
			return err
		}
		if len(raw) > 2 { // anything beyond "{}"
			var bp domain.Blueprint
			if err := json.Unmarshal(raw, &bp); err != nil {
				return fmt.Errorf("stored draft is corrupt: %w", err)
			}
			draft = &bp
		}
		rows, err := tx.Query(ctx, `SELECT role, content FROM genesis_messages WHERE tenant_id=$1 AND session_id=$2 ORDER BY id`, tenant, session)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m Message
			if err := rows.Scan(&m.Role, &m.Content); err != nil {
				return err
			}
			msgs = append(msgs, m)
		}
		return rows.Err()
	})
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

// RecordTurn appends the founder's message and the assistant's reply, and stores the new draft if there is one,
// atomically. turnKey makes a retried request idempotent: replaying it returns replayed=true and changes nothing.
func (s *PG) RecordTurn(ctx context.Context, tenant, session uuid.UUID, turnKey, founderMsg, assistantMsg string, draft *domain.Blueprint) (replayed bool, err error) {
	err = s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`INSERT INTO genesis_messages (tenant_id, session_id, role, content, idempotency_key) VALUES ($1,$2,'founder',$3,$4)
			 ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`, tenant, session, founderMsg, turnKey+":f")
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			replayed = true
			return nil
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO genesis_messages (tenant_id, session_id, role, content, idempotency_key) VALUES ($1,$2,'assistant',$3,$4)`,
			tenant, session, assistantMsg, turnKey+":a"); err != nil {
			return err
		}
		if draft != nil {
			raw, err := json.Marshal(draft)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE genesis_sessions SET draft_blueprint=$3 WHERE tenant_id=$1 AND id=$2`, tenant, session, raw); err != nil {
				return err
			}
		}
		return nil
	})
	return replayed, err
}

// ───────────────────────────── blueprints ─────────────────────────────

// StoredBlueprint is a persisted blueprint version.
type StoredBlueprint struct {
	ID         uuid.UUID
	Version    int
	Status     string
	Blueprint  domain.Blueprint
	Hash       [32]byte
	ApprovedBy string
	ApprovedAt *time.Time
}

// SaveBlueprint stores bp as the next DRAFT version for the tenant. Replaying key returns the existing row.
func (s *PG) SaveBlueprint(ctx context.Context, tenant uuid.UUID, session *uuid.UUID, bp domain.Blueprint, createdBy, key string) (StoredBlueprint, error) {
	if err := bp.Validate(); err != nil {
		return StoredBlueprint{}, err
	}
	// Refuse now a blueprint whose skills could not be generated, rather than after the founder has approved it.
	if err := skillgen.Check(bp); err != nil {
		return StoredBlueprint{}, err
	}
	raw, err := bp.CanonicalJSON()
	if err != nil {
		return StoredBlueprint{}, err
	}
	var out StoredBlueprint
	err = s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		var existing uuid.UUID
		switch err := tx.QueryRow(ctx, `SELECT id FROM company_blueprints WHERE tenant_id=$1 AND idempotency_key=$2`, tenant, key).Scan(&existing); {
		case err == nil:
			var e error
			out, e = loadBlueprint(ctx, tx, tenant, existing)
			return e
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO company_blueprints (tenant_id, version, schema_version, blueprint, session_id, created_by, idempotency_key)
			 VALUES ($1, coalesce((SELECT max(version) FROM company_blueprints WHERE tenant_id=$1),0)+1, $2, $3, $4, $5, $6)
			 RETURNING id`, tenant, bp.SchemaVersion, raw, session, createdBy, key).Scan(&id); err != nil {
			return err
		}
		var e error
		out, e = loadBlueprint(ctx, tx, tenant, id)
		return e
	})
	return out, err
}

func loadBlueprint(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID) (StoredBlueprint, error) {
	var sb StoredBlueprint
	var raw []byte
	var approvedBy *string
	err := tx.QueryRow(ctx,
		`SELECT id, version, status, blueprint, approved_by, approved_at FROM company_blueprints WHERE tenant_id=$1 AND id=$2`,
		tenant, id).Scan(&sb.ID, &sb.Version, &sb.Status, &raw, &approvedBy, &sb.ApprovedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return sb, ErrNotFound
	}
	if err != nil {
		return sb, err
	}
	if approvedBy != nil {
		sb.ApprovedBy = *approvedBy
	}
	if err := json.Unmarshal(raw, &sb.Blueprint); err != nil {
		return sb, fmt.Errorf("stored blueprint is corrupt: %w", err)
	}
	if sb.Hash, err = sb.Blueprint.Hash(); err != nil {
		return sb, err
	}
	return sb, nil
}

// Blueprint loads one blueprint version by id.
func (s *PG) Blueprint(ctx context.Context, tenant, id uuid.UUID) (StoredBlueprint, error) {
	var sb StoredBlueprint
	err := s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		var e error
		sb, e = loadBlueprint(ctx, tx, tenant, id)
		return e
	})
	return sb, err
}

// LatestBlueprint returns the highest-version blueprint of the tenant.
func (s *PG) LatestBlueprint(ctx context.Context, tenant uuid.UUID) (StoredBlueprint, error) {
	var sb StoredBlueprint
	err := s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM company_blueprints WHERE tenant_id=$1 ORDER BY version DESC LIMIT 1`, tenant).Scan(&id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		var e error
		sb, e = loadBlueprint(ctx, tx, tenant, id)
		return e
	})
	return sb, err
}

// ApproveBlueprint freezes a DRAFT blueprint as the tenant's approved plan and applies its identity (slug, name,
// country, currency, ...) to the organization. Approving the already-approved blueprint is a no-op. A tenant that
// is already ACTIVE cannot swap blueprints here.
func (s *PG) ApproveBlueprint(ctx context.Context, tenant, id uuid.UUID, approvedBy string) (StoredBlueprint, error) {
	var out StoredBlueprint
	err := s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		sb, err := loadBlueprint(ctx, tx, tenant, id)
		if err != nil {
			return err
		}
		if sb.Status == "APPROVED" {
			out = sb
			return nil
		}
		if sb.Status != "DRAFT" {
			return fmt.Errorf("%w: blueprint is %s", ErrConflict, sb.Status)
		}
		if err := sb.Blueprint.Validate(); err != nil {
			return err
		}
		var orgStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM organizations WHERE id=$1`, tenant).Scan(&orgStatus); err != nil {
			return err
		}
		if orgStatus == "ACTIVE" {
			return ErrTenantLive
		}
		if _, err := tx.Exec(ctx, `UPDATE company_blueprints SET status='SUPERSEDED' WHERE tenant_id=$1 AND status='APPROVED'`, tenant); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE company_blueprints SET status='APPROVED', approved_by=$3, approved_at=now() WHERE tenant_id=$1 AND id=$2`,
			tenant, id, approvedBy); err != nil {
			return err
		}
		b := sb.Blueprint.Business
		if _, err := tx.Exec(ctx,
			`UPDATE organizations SET slug=$2, name=$3, industry=$4, country=$5, base_currency=$6, timezone=$7 WHERE id=$1`,
			tenant, b.Slug, b.Name, b.Industry, b.Country, b.BaseCurrency, b.Timezone); err != nil {
			if code, _ := pgCode(err); code == "23505" {
				return ErrSlugTaken
			}
			return err
		}
		out, err = loadBlueprint(ctx, tx, tenant, id)
		return err
	})
	return out, err
}

// ───────────────────────────── provisioner.Store ─────────────────────────────

var _ provisioner.Store = (*PG)(nil)

func (s *PG) BeginRun(ctx context.Context, tenant, blueprint uuid.UUID, key string, lease time.Duration) (provisioner.Run, bool, error) {
	var run provisioner.Run
	claimed := false
	err := s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		var updated time.Time
		var errMsg *string
		var now time.Time
		err := tx.QueryRow(ctx,
			`SELECT id, blueprint_id, status, error, updated_at, now() FROM provision_runs
			 WHERE tenant_id=$1 AND idempotency_key=$2 FOR UPDATE`, tenant, key).
			Scan(&run.ID, &run.BlueprintID, &run.Status, &errMsg, &updated, &now)
		switch {
		case err == nil:
			run.TenantID = tenant
			if errMsg != nil {
				run.Error = *errMsg
			}
			switch run.Status {
			case provisioner.RunPending, provisioner.RunRunning, provisioner.RunCompensating:
				if now.Sub(updated) >= lease {
					if _, err := tx.Exec(ctx, `UPDATE provision_runs SET updated_at=now() WHERE id=$1 AND tenant_id=$2`, run.ID, tenant); err != nil {
						return err
					}
					claimed = true
				}
			}
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		// Only approved blueprints may be provisioned.
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM company_blueprints WHERE tenant_id=$1 AND id=$2`, tenant, blueprint).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if status != "APPROVED" {
			return ErrNotApproved
		}
		run = provisioner.Run{TenantID: tenant, BlueprintID: blueprint, Status: provisioner.RunPending}
		if err := tx.QueryRow(ctx,
			`INSERT INTO provision_runs (tenant_id, blueprint_id, idempotency_key) VALUES ($1,$2,$3) RETURNING id`,
			tenant, blueprint, key).Scan(&run.ID); err != nil {
			if code, c := pgCode(err); code == "23505" && c == "provision_runs_one_active" {
				return ErrTenantBusy
			}
			return err
		}
		claimed = true
		return nil
	})
	return run, claimed, err
}

func (s *PG) Steps(ctx context.Context, tenant, run uuid.UUID) ([]provisioner.StepRecord, error) {
	var out []provisioner.StepRecord
	err := s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT step_key, ordinal, status, attempts, coalesce(external_ref->>'ref',''), coalesce(error,'')
			 FROM provision_steps WHERE tenant_id=$1 AND run_id=$2 ORDER BY ordinal`, tenant, run)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r provisioner.StepRecord
			var ref string
			if err := rows.Scan(&r.Key, &r.Ordinal, &r.Status, &r.Attempts, &ref, &r.Error); err != nil {
				return err
			}
			r.Ref = ports.Ref(ref)
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PG) SaveStep(ctx context.Context, tenant, run uuid.UUID, r provisioner.StepRecord) error {
	ref, _ := json.Marshal(map[string]string{"ref": string(r.Ref)})
	return s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO provision_steps (tenant_id, run_id, step_key, ordinal, status, attempts, external_ref, error, started_at, finished_at, compensated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,nullif($8,''),
			         CASE WHEN $5 = 'RUNNING' THEN now() END,
			         CASE WHEN $5 IN ('SUCCEEDED','FAILED') THEN now() END,
			         CASE WHEN $5 = 'COMPENSATED' THEN now() END)
			 ON CONFLICT (run_id, step_key) DO UPDATE SET
			   status = EXCLUDED.status,
			   attempts = GREATEST(provision_steps.attempts, EXCLUDED.attempts),
			   external_ref = CASE WHEN EXCLUDED.external_ref->>'ref' <> '' THEN EXCLUDED.external_ref ELSE provision_steps.external_ref END,
			   error = EXCLUDED.error,
			   started_at = coalesce(provision_steps.started_at, EXCLUDED.started_at),
			   finished_at = coalesce(EXCLUDED.finished_at, provision_steps.finished_at),
			   compensated_at = coalesce(EXCLUDED.compensated_at, provision_steps.compensated_at)`,
			tenant, run, r.Key, r.Ordinal, string(r.Status), r.Attempts, ref, r.Error); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE provision_runs SET updated_at=now() WHERE id=$1 AND tenant_id=$2`, run, tenant)
		return err
	})
}

func (s *PG) SetRunStatus(ctx context.Context, tenant, run uuid.UUID, status provisioner.RunStatus) error {
	return s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`UPDATE provision_runs SET status=$3, started_at = coalesce(started_at, CASE WHEN $3='RUNNING' THEN now() END)
			 WHERE id=$1 AND tenant_id=$2`, run, tenant, string(status))
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 1 {
			return ErrNotFound
		}
		return nil
	})
}

func (s *PG) FinishRun(ctx context.Context, tenant, run uuid.UUID, status provisioner.RunStatus, errMsg string) error {
	return s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`UPDATE provision_runs SET status=$3, error=nullif($4,''), finished_at=now() WHERE id=$1 AND tenant_id=$2`,
			run, tenant, string(status), errMsg)
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 1 {
			return ErrNotFound
		}
		return nil
	})
}

func (s *PG) SetTenantStatus(ctx context.Context, tenant uuid.UUID, status string) error {
	return s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `UPDATE organizations SET status=$2 WHERE id=$1`, tenant, status)
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 1 {
			return ErrNotFound
		}
		return nil
	})
}

func (s *PG) RecordIntegrations(ctx context.Context, tenant uuid.UUID, refs map[string]ports.Ref) error {
	return s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		for system, ref := range refs {
			if ref == "" {
				return fmt.Errorf("integration %s has an empty reference", system)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO tenant_integrations (tenant_id, system, external_id) VALUES ($1,$2,$3)
				 ON CONFLICT (tenant_id, system) DO UPDATE SET external_id=EXCLUDED.external_id, status='ACTIVE'`,
				tenant, system, string(ref)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *PG) Audit(ctx context.Context, tenant uuid.UUID, action string, data map[string]any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO audit_logs (tenant_id, actor_type, actor_id, action, entity_type, entity_id, data)
			 VALUES ($1,'SYSTEM','provisioner',$2,'tenant',$4,$3)`, tenant, action, raw, tenant.String())
		return err
	})
}

// AuditUser records an action by a human.
func (s *PG) AuditUser(ctx context.Context, tenant uuid.UUID, sub, action, entityType, entityID string, data map[string]any) error {
	return s.AuditActor(ctx, tenant, "USER", sub, action, entityType, entityID, data)
}

// AuditActor records an action by a USER, AGENT or SYSTEM actor. Audit rows are append-only.
func (s *PG) AuditActor(ctx context.Context, tenant uuid.UUID, actorType, actorID, action, entityType, entityID string, data map[string]any) error {
	if data == nil {
		data = map[string]any{} // the column requires a JSON object; nil would marshal to null and be refused
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO audit_logs (tenant_id, actor_type, actor_id, action, entity_type, entity_id, data)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)`, tenant, actorType, actorID, action, entityType, entityID, raw)
		return err
	})
}

// auditInTx appends an audit row inside the caller's transaction, so the action it records and the record of it commit or
// roll back together. Use it for the actions where a missing record would defeat the point of auditing them (approving money,
// changing a safety ceiling); AuditActor, which runs in its own transaction, is for the rest.
func auditInTx(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, actorType, actorID, action, entityType, entityID string, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO audit_logs (tenant_id, actor_type, actor_id, action, entity_type, entity_id, data)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`, tenant, actorType, actorID, action, entityType, entityID, raw)
	return err
}

// ───────────────────────────── seeding ─────────────────────────────

// SeedTenant writes the blueprint into the tenant's own tables, atomically and idempotently. It refuses to run
// unless the stored blueprint is APPROVED and byte-for-byte (by hash) the one being seeded, and unless every skill
// hashes to its content: whatever the founder approved is exactly what gets installed.
func (s *PG) SeedTenant(ctx context.Context, tenant, blueprintID uuid.UUID, bp domain.Blueprint, skills []ports.Skill) error {
	want, err := bp.Hash()
	if err != nil {
		return err
	}
	for _, sk := range skills {
		if sha256.Sum256([]byte(sk.Markdown)) != sk.SHA256 {
			return fmt.Errorf("skill %q does not match its hash", sk.AgentKey)
		}
	}
	skillByAgent := map[string]ports.Skill{}
	for _, sk := range skills {
		skillByAgent[sk.AgentKey] = sk
	}

	return s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		stored, err := loadBlueprint(ctx, tx, tenant, blueprintID)
		if err != nil {
			return err
		}
		if stored.Status != "APPROVED" {
			return ErrNotApproved
		}
		if stored.Hash != want {
			return fmt.Errorf("%w: seeded blueprint differs from the approved one", ErrConflict)
		}
		key := func(kind, name string) string { return fmt.Sprintf("seed:%s:%s:%s", blueprintID, kind, name) }

		// Chart of accounts: parents first via a second pass, so insertion order doesn't matter.
		var codes []string
		for _, a := range bp.Accounting.ChartOfAccounts {
			codes = append(codes, a.Code)
			var existing *string
			err := tx.QueryRow(ctx, `SELECT type FROM ledger_accounts WHERE tenant_id=$1 AND code=$2`, tenant, a.Code).Scan(&existing)
			if err == nil && existing != nil && *existing != string(a.Type) {
				return fmt.Errorf("%w: account %s already exists as %s, blueprint says %s", ErrConflict, a.Code, *existing, a.Type)
			}
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO ledger_accounts (tenant_id, code, name, type, is_postable, active) VALUES ($1,$2,$3,$4,$5,true)
				 ON CONFLICT (tenant_id, code) DO UPDATE SET name=EXCLUDED.name, is_postable=EXCLUDED.is_postable, active=true`,
				tenant, a.Code, a.Name, string(a.Type), a.Postable); err != nil {
				return err
			}
		}
		for _, a := range bp.Accounting.ChartOfAccounts {
			if _, err := tx.Exec(ctx,
				`UPDATE ledger_accounts SET parent_id = (SELECT id FROM ledger_accounts WHERE tenant_id=$1 AND code=nullif($3,''))
				 WHERE tenant_id=$1 AND code=$2`, tenant, a.Code, a.ParentCode); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE ledger_accounts SET active=false WHERE tenant_id=$1 AND NOT (code = ANY($2))`, tenant, codes); err != nil {
			return err
		}

		// Pricing rules.
		var ruleNames []string
		for _, r := range bp.Pricing {
			ruleNames = append(ruleNames, r.Name)
			if _, err := tx.Exec(ctx,
				`INSERT INTO tenant_pricing_rules (tenant_id, name, version, model, currency, formula, margin_floor_bps, margin_target_bps,
				                                   approval_threshold_minor, status, blueprint_id, created_by, idempotency_key)
				 VALUES ($1,$2,1,$3,$4,$5,$6,$7,$8,'ACTIVE',$9,'provisioner',$10)
				 ON CONFLICT (tenant_id, name, version) DO UPDATE SET model=EXCLUDED.model, currency=EXCLUDED.currency, formula=EXCLUDED.formula,
				   margin_floor_bps=EXCLUDED.margin_floor_bps, margin_target_bps=EXCLUDED.margin_target_bps,
				   approval_threshold_minor=EXCLUDED.approval_threshold_minor, status='ACTIVE', blueprint_id=EXCLUDED.blueprint_id,
				   idempotency_key=EXCLUDED.idempotency_key`,
				tenant, r.Name, string(r.Model), r.Currency, []byte(r.Formula), r.MarginFloorBps, r.MarginTargetBps,
				r.ApprovalThresholdMinor, blueprintID, key("pricing", r.Name)); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE tenant_pricing_rules SET status='RETIRED' WHERE tenant_id=$1 AND status<>'RETIRED' AND NOT (name = ANY($2))`, tenant, ruleNames); err != nil {
			return err
		}

		// CRM pipelines (rows are configuration, so stale ones are simply removed).
		var pipelineKeys []string
		for i, p := range bp.CRM.Pipelines {
			pipelineKeys = append(pipelineKeys, p.Key)
			stages, _ := json.Marshal(p.Stages)
			fields, _ := json.Marshal(nonNil(p.CustomFields))
			if _, err := tx.Exec(ctx,
				`INSERT INTO crm_pipelines (tenant_id, key, name, entity_type, stages, custom_fields, status, blueprint_id, idempotency_key, position)
				 VALUES ($1,$2,$3,$4,$5,$6,'ACTIVE',$7,$8,$9)
				 ON CONFLICT (tenant_id, key) DO UPDATE SET name=EXCLUDED.name, entity_type=EXCLUDED.entity_type, stages=EXCLUDED.stages,
				   custom_fields=EXCLUDED.custom_fields, status='ACTIVE', blueprint_id=EXCLUDED.blueprint_id, idempotency_key=EXCLUDED.idempotency_key,
				   position=EXCLUDED.position`,
				tenant, p.Key, p.Name, string(p.EntityType), stages, fields, blueprintID, key("pipeline", p.Key), i); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM crm_pipelines WHERE tenant_id=$1 AND NOT (key = ANY($2))`, tenant, pipelineKeys); err != nil {
			return err
		}

		// Workflows.
		var wfKeys []string
		for _, w := range bp.Workflows {
			wfKeys = append(wfKeys, w.Key)
			trigger, _ := json.Marshal(map[string]string{"event": w.Trigger})
			def, _ := json.Marshal(map[string]any{"steps": w.Steps})
			if _, err := tx.Exec(ctx,
				`INSERT INTO workflows (tenant_id, key, version, name, trigger, definition, requires_approval, status, blueprint_id, idempotency_key)
				 VALUES ($1,$2,1,$3,$4,$5,$6,'ACTIVE',$7,$8)
				 ON CONFLICT (tenant_id, key, version) DO UPDATE SET name=EXCLUDED.name, trigger=EXCLUDED.trigger, definition=EXCLUDED.definition,
				   requires_approval=EXCLUDED.requires_approval, status='ACTIVE', blueprint_id=EXCLUDED.blueprint_id, idempotency_key=EXCLUDED.idempotency_key`,
				tenant, w.Key, w.Name, trigger, def, w.RequiresApproval, blueprintID, key("workflow", w.Key)); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE workflows SET status='RETIRED' WHERE tenant_id=$1 AND status<>'RETIRED' AND NOT (key = ANY($2))`, tenant, wfKeys); err != nil {
			return err
		}

		// Agents and their skills. The blueprint's founder approval covers its skills; the DB trigger forces a
		// changed skill through PENDING_APPROVAL, which we then resolve in a second statement.
		var agentKeys []string
		if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
			return err
		}
		for _, a := range bp.Agents {
			agentKeys = append(agentKeys, a.Key)
			sk, ok := skillByAgent[a.Key]
			if !ok {
				return fmt.Errorf("no skill rendered for agent %q", a.Key)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO agent_org_configs (tenant_id, agent_key, role_title, reports_to_agent_key, skill_md, skill_status, tools_allowed,
				                                monthly_budget_minor, budget_currency, status, blueprint_id, idempotency_key)
				 VALUES ($1,$2,$3,nullif($4,''),$5,'PENDING_APPROVAL',$6,$7,$8,'PLANNED',$9,$10)
				 ON CONFLICT (tenant_id, agent_key) DO UPDATE SET role_title=EXCLUDED.role_title, reports_to_agent_key=EXCLUDED.reports_to_agent_key,
				   skill_md=EXCLUDED.skill_md, skill_status='PENDING_APPROVAL', tools_allowed=EXCLUDED.tools_allowed,
				   monthly_budget_minor=EXCLUDED.monthly_budget_minor, budget_currency=EXCLUDED.budget_currency, status='PLANNED',
				   blueprint_id=EXCLUDED.blueprint_id, idempotency_key=EXCLUDED.idempotency_key`,
				tenant, a.Key, a.RoleTitle, a.ReportsTo, sk.Markdown, a.Tools, a.MonthlyBudgetMinor, bp.Business.BaseCurrency,
				blueprintID, key("agent", a.Key)); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM agent_org_configs WHERE tenant_id=$1 AND NOT (agent_key = ANY($2))`, tenant, agentKeys); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE agent_org_configs SET skill_status='APPROVED' WHERE tenant_id=$1 AND blueprint_id=$2 AND skill_status='PENDING_APPROVAL'`,
			tenant, blueprintID); err != nil {
			return err
		}
		return nil
	})
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// RetireSeed undoes SeedTenant for compensation: configuration rows are removed, and rows that other records may
// reference (accounts, pricing rules, workflows) are deactivated rather than deleted.
func (s *PG) RetireSeed(ctx context.Context, tenant uuid.UUID) error {
	return s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		for _, q := range []string{
			`UPDATE ledger_accounts SET active=false WHERE tenant_id=$1`,
			`UPDATE tenant_pricing_rules SET status='RETIRED' WHERE tenant_id=$1 AND status<>'RETIRED'`,
			`UPDATE workflows SET status='RETIRED' WHERE tenant_id=$1 AND status<>'RETIRED'`,
			`DELETE FROM crm_pipelines WHERE tenant_id=$1`,
			`DELETE FROM agent_org_configs WHERE tenant_id=$1`,
			`DELETE FROM tenant_integrations WHERE tenant_id=$1`,
		} {
			if _, err := tx.Exec(ctx, q, tenant); err != nil {
				return err
			}
		}
		return nil
	})
}

// ───────────────────────────── queries used by the API layer ─────────────────────────────

// ProvisionRun is a run with its steps, for status display.
type ProvisionRun struct {
	Run   provisioner.Run
	Steps []provisioner.StepRecord
}

// LatestRun returns the tenant's most recent provisioning run.
func (s *PG) LatestRun(ctx context.Context, tenant uuid.UUID) (ProvisionRun, error) {
	var pr ProvisionRun
	err := s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		var errMsg *string
		err := tx.QueryRow(ctx,
			`SELECT id, blueprint_id, status, error FROM provision_runs WHERE tenant_id=$1 ORDER BY created_at DESC, id DESC LIMIT 1`, tenant).
			Scan(&pr.Run.ID, &pr.Run.BlueprintID, &pr.Run.Status, &errMsg)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		pr.Run.TenantID = tenant
		if errMsg != nil {
			pr.Run.Error = *errMsg
		}
		return nil
	})
	if err != nil {
		return pr, err
	}
	pr.Steps, err = s.Steps(ctx, tenant, pr.Run.ID)
	return pr, err
}

// TurnReply returns the stored assistant reply of an already-recorded Genesis turn, so a retried request can be
// answered without calling the model again.
func (s *PG) TurnReply(ctx context.Context, tenant uuid.UUID, turnKey string) (reply string, found bool, err error) {
	err = s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		e := tx.QueryRow(ctx, `SELECT content FROM genesis_messages WHERE tenant_id=$1 AND idempotency_key=$2`, tenant, turnKey+":a").Scan(&reply)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		found = e == nil
		return e
	})
	return reply, found, err
}

// AddMember grants a user a role in a tenant (idempotent; an existing role is updated). Authorisation is the
// caller's job.
func (s *PG) AddMember(ctx context.Context, tenant uuid.UUID, userSub, role string) error {
	return s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tenant_members (tenant_id, user_sub, role) VALUES ($1,$2,$3)
			 ON CONFLICT (tenant_id, user_sub) DO UPDATE SET role=EXCLUDED.role`, tenant, userSub, role)
		return err
	})
}

// EnsureRestrictedRole fails if the pool's database role could bypass row-level security (superuser or
// BYPASSRLS). The control plane calls it at startup in production: connecting as such a role would silently
// disable tenant isolation.
func EnsureRestrictedRole(ctx context.Context, pool *pgxpool.Pool) error {
	var role string
	var unrestricted bool
	if err := pool.QueryRow(ctx,
		`SELECT current_user, rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&role, &unrestricted); err != nil {
		return fmt.Errorf("store: inspecting database role: %w", err)
	}
	if unrestricted {
		return fmt.Errorf("store: database role %q is a superuser or has BYPASSRLS; connect as venture_app so row-level security applies", role)
	}
	return nil
}

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mrbiz/venture-os/internal/domain"
	"github.com/mrbiz/venture-os/internal/tenancy"
)

var (
	// ErrIdempotencyMismatch means an idempotency key was reused for a different request.
	ErrIdempotencyMismatch = errors.New("idempotency key already used for a different request")
	// ErrAlreadyDecided means the approval is no longer pending.
	ErrAlreadyDecided = errors.New("approval has already been decided")
	// ErrHashMismatch means the caller approved a payload other than the stored one.
	ErrHashMismatch = errors.New("payload hash does not match the stored request")
)

func pricingModel(s string) domain.PricingModel { return domain.PricingModel(s) }

// Approval is a row of approval_queue.
type Approval struct {
	ID              uuid.UUID
	TenantID        uuid.UUID
	BusinessName    string
	Kind            string
	Status          string
	Title           string
	Summary         string
	RequestedByType string
	RequestedBy     string
	Risk            string
	Payload         json.RawMessage
	PayloadHash     [32]byte
	AmountMinor     *int64
	Currency        string
	DecidedBy       string
	DecisionNote    string
	ExecutionResult json.RawMessage
	CreatedAt       time.Time
}

// NewApproval is a request to queue.
type NewApproval struct {
	Kind            string
	Title           string
	Summary         string
	RequestedByType string // AGENT | USER | SYSTEM
	RequestedBy     string
	Risk            string
	Payload         json.RawMessage
	AmountMinor     *int64
	Currency        string
	RelatedType     string
	RelatedID       string
	IdempotencyKey  string
}

const approvalCols = `a.id, a.tenant_id, coalesce(o.name,''), a.kind, a.status, a.title, a.summary, a.requested_by_type, a.requested_by,
	a.risk_level, a.payload, a.payload_hash, a.amount_minor, coalesce(a.currency,''), coalesce(a.decided_by,''),
	coalesce(a.decision_note,''), a.execution_result, a.created_at`

func scanApproval(row pgx.Row) (Approval, error) {
	var a Approval
	var hash, payload, result []byte
	if err := row.Scan(&a.ID, &a.TenantID, &a.BusinessName, &a.Kind, &a.Status, &a.Title, &a.Summary, &a.RequestedByType,
		&a.RequestedBy, &a.Risk, &payload, &hash, &a.AmountMinor, &a.Currency, &a.DecidedBy, &a.DecisionNote, &result, &a.CreatedAt); err != nil {
		return Approval{}, err
	}
	a.Payload, a.ExecutionResult = payload, result
	if len(hash) == 32 {
		copy(a.PayloadHash[:], hash)
	}
	return a, nil
}

// CreateApproval queues a request as PENDING_APPROVAL. It is idempotent per (tenant, key): replaying returns the
// original; reusing the key with a different kind or payload is ErrIdempotencyMismatch.
func (s *PG) CreateApproval(ctx context.Context, tenant uuid.UUID, n NewApproval) (a Approval, created bool, err error) {
	if (n.AmountMinor == nil) != (n.Currency == "") {
		return Approval{}, false, errors.New("amount and currency must be given together")
	}
	err = s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		var id uuid.UUID
		e := tx.QueryRow(ctx,
			`INSERT INTO approval_queue (tenant_id, kind, title, summary, requested_by_type, requested_by, risk_level, payload,
			                             amount_minor, currency, related_entity_type, related_entity_id, idempotency_key)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,nullif($10,''),nullif($11,''),nullif($12,''),$13)
			 ON CONFLICT (tenant_id, idempotency_key) DO NOTHING RETURNING id`,
			tenant, n.Kind, n.Title, n.Summary, n.RequestedByType, n.RequestedBy, n.Risk, []byte(n.Payload),
			n.AmountMinor, n.Currency, n.RelatedType, n.RelatedID, n.IdempotencyKey).Scan(&id)
		switch {
		case e == nil:
			created = true
		case errors.Is(e, pgx.ErrNoRows):
			var same bool
			if e := tx.QueryRow(ctx,
				`SELECT id, kind = $3 AND payload = $4::jsonb AND amount_minor IS NOT DISTINCT FROM $5::bigint
				 FROM approval_queue WHERE tenant_id=$1 AND idempotency_key=$2`,
				tenant, n.IdempotencyKey, n.Kind, []byte(n.Payload), n.AmountMinor).Scan(&id, &same); e != nil {
				return e
			}
			if !same {
				return ErrIdempotencyMismatch
			}
		default:
			return e
		}
		a, e = scanApproval(tx.QueryRow(ctx,
			`SELECT `+approvalCols+` FROM approval_queue a LEFT JOIN organizations o ON o.id=a.tenant_id WHERE a.tenant_id=$1 AND a.id=$2`, tenant, id))
		return e
	})
	return a, created, err
}

// Approval loads one approval.
func (s *PG) Approval(ctx context.Context, tenant, id uuid.UUID) (Approval, error) {
	var a Approval
	err := s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		var e error
		a, e = scanApproval(tx.QueryRow(ctx,
			`SELECT `+approvalCols+` FROM approval_queue a LEFT JOIN organizations o ON o.id=a.tenant_id WHERE a.tenant_id=$1 AND a.id=$2`, tenant, id))
		return e
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Approval{}, ErrNotFound
	}
	return a, err
}

// ListApprovals returns approvals with the given status across `tenants` (a read-only portfolio scope), newest
// first. The caller must have verified the user's membership for every tenant passed.
func (s *PG) ListApprovals(ctx context.Context, tenants []uuid.UUID, status string, limit int) ([]Approval, error) {
	if len(tenants) == 0 {
		return nil, nil
	}
	var out []Approval
	err := s.tx(ctx, tenancy.Scope{ReadTenantIDs: tenants}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+approvalCols+` FROM approval_queue a LEFT JOIN organizations o ON o.id=a.tenant_id
			 WHERE a.status=$1 ORDER BY a.created_at DESC, a.id LIMIT $2`, status, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanApproval(rows)
			if err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// DecideApproval moves a PENDING_APPROVAL request to APPROVED or REJECTED. When approving, wantHash must equal the
// stored payload hash (the decider approves exactly what they saw). Deciding twice returns ErrAlreadyDecided.
func (s *PG) DecideApproval(ctx context.Context, tenant, id uuid.UUID, approve bool, by, note string, wantHash [32]byte) (Approval, error) {
	var a Approval
	err := s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		cur, err := scanApproval(tx.QueryRow(ctx,
			`SELECT `+approvalCols+` FROM approval_queue a LEFT JOIN organizations o ON o.id=a.tenant_id
			 WHERE a.tenant_id=$1 AND a.id=$2 FOR UPDATE OF a`, tenant, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur.Status != "PENDING_APPROVAL" {
			a = cur
			return ErrAlreadyDecided
		}
		if approve && cur.PayloadHash != wantHash {
			return ErrHashMismatch
		}
		if approve {
			// Hard ceiling on money leaving the business: checked in this transaction, under a per-tenant lock.
			if err := checkSpend(ctx, tx, tenant, cur.Kind, cur.Currency, cur.AmountMinor); err != nil {
				return err
			}
		}
		status := "REJECTED"
		if approve {
			status = "APPROVED"
		}
		if _, err := tx.Exec(ctx,
			`UPDATE approval_queue SET status=$3, decided_by=$4, decided_at=now(), decision_note=nullif($5,'')
			 WHERE tenant_id=$1 AND id=$2 AND status='PENDING_APPROVAL'`, tenant, id, status, by, note); err != nil {
			return err
		}
		a, err = scanApproval(tx.QueryRow(ctx,
			`SELECT `+approvalCols+` FROM approval_queue a LEFT JOIN organizations o ON o.id=a.tenant_id WHERE a.tenant_id=$1 AND a.id=$2`, tenant, id))
		return err
	})
	return a, err
}

// ExecutionLease is how long an EXECUTING claim is respected. A claim older than this belongs to a run that crashed (or
// lost its connection) before recording an outcome, so it may be taken over. It must comfortably exceed the time one
// execution is allowed to take (controlplane.executionBudget). Executors are idempotent, so a takeover that overlaps a
// slow original run cannot duplicate its effects.
const ExecutionLease = 5 * time.Minute

// ClaimExecution atomically moves an APPROVED (or previously FAILED) approval to EXECUTING, or takes over an EXECUTING
// claim whose lease has expired. Only one caller can win, which is what makes execution at-most-once-at-a-time.
func (s *PG) ClaimExecution(ctx context.Context, tenant, id uuid.UUID) (bool, error) {
	var won bool
	err := s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		// updated_at is refreshed by the approval_queue trigger on every update, so it marks when the claim was taken.
		ct, err := tx.Exec(ctx,
			`UPDATE approval_queue SET status='EXECUTING'
			  WHERE tenant_id=$1 AND id=$2
			    AND (status IN ('APPROVED','FAILED') OR (status='EXECUTING' AND updated_at < now() - $3::interval))`,
			tenant, id, fmt.Sprintf("%d seconds", int(ExecutionLease.Seconds())))
		won = err == nil && ct.RowsAffected() == 1
		return err
	})
	return won, err
}

// FinishExecution records the outcome of an execution that was claimed.
func (s *PG) FinishExecution(ctx context.Context, tenant, id uuid.UUID, ok bool, result json.RawMessage) error {
	status := "FAILED"
	if ok {
		status = "EXECUTED"
	}
	return s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`UPDATE approval_queue SET status=$3, executed_at=now(), execution_result=$4 WHERE tenant_id=$1 AND id=$2 AND status='EXECUTING'`,
			tenant, id, status, []byte(result))
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 1 {
			return fmt.Errorf("%w: approval %s is not executing", ErrConflict, id)
		}
		return nil
	})
}

// HashHex formats a payload hash.
func HashHex(h [32]byte) string { return fmt.Sprintf("%x", h[:]) }

// PendingCount summarises a tenant's open approvals.
type PendingCount struct{ Total, High int }

// PendingCounts counts PENDING_APPROVAL items per tenant (a read-only portfolio scope). The caller must have
// verified the user may see approvals for every tenant passed.
func (s *PG) PendingCounts(ctx context.Context, tenants []uuid.UUID) (map[uuid.UUID]PendingCount, error) {
	out := map[uuid.UUID]PendingCount{}
	if len(tenants) == 0 {
		return out, nil
	}
	err := s.tx(ctx, tenancy.Scope{ReadTenantIDs: tenants}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, count(*)::int, (count(*) FILTER (WHERE risk_level='HIGH'))::int
			 FROM approval_queue WHERE status='PENDING_APPROVAL' GROUP BY tenant_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			var c PendingCount
			if err := rows.Scan(&id, &c.Total, &c.High); err != nil {
				return err
			}
			out[id] = c
		}
		return rows.Err()
	})
	return out, err
}

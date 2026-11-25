package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mrbiz/venture-os/internal/vault"
)

// ───────────────────────────── spend caps ─────────────────────────────

// ErrSpendCap is matched (errors.Is) by *SpendCapError.
var ErrSpendCap = errors.New("spend cap exceeded")

// SpendCapError says which ceiling an approval would break, and by how much, in minor units.
type SpendCapError struct {
	Currency  string
	Dimension string // "per transaction", "per hour" or "per day"
	Cap       int64
	Spent     int64 // already approved in the window (0 for per transaction)
	Requested int64
}

func (e *SpendCapError) Error() string {
	if e.Dimension == "per transaction" {
		return fmt.Sprintf("spend cap: %d %s minor units exceeds the per-transaction cap of %d", e.Requested, e.Currency, e.Cap)
	}
	return fmt.Sprintf("spend cap: %d already approved %s plus %d would exceed the cap of %d (%s, minor units)", e.Spent, e.Dimension, e.Requested, e.Cap, e.Currency)
}
func (e *SpendCapError) Is(target error) bool { return target == ErrSpendCap }

// SpendCap is a business's ceilings on money leaving it, in one currency. A nil ceiling means none on that dimension.
type SpendCap struct {
	Currency       string
	PerTransaction *int64
	PerHour        *int64
	PerDay         *int64
	UpdatedBy      string
	UpdatedAt      time.Time
}

// moneyOutKinds are the approval kinds whose approval commits money to leave the business.
const moneyOutKinds = `('PAYMENT','PURCHASE_ORDER')`

// SpendCaps lists the tenant's caps.
func (s *PG) SpendCaps(ctx context.Context, tenant uuid.UUID) ([]SpendCap, error) {
	var out []SpendCap
	err := s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT currency, per_transaction_minor, per_hour_minor, per_day_minor, updated_by, updated_at FROM spend_caps WHERE tenant_id=$1 ORDER BY currency`, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c SpendCap
			if err := rows.Scan(&c.Currency, &c.PerTransaction, &c.PerHour, &c.PerDay, &c.UpdatedBy, &c.UpdatedAt); err != nil {
				return err
			}
			c.UpdatedAt = c.UpdatedAt.UTC()
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// SetSpendCap creates or replaces the cap for one currency. Setting all three ceilings to nil removes the caps.
func (s *PG) SetSpendCap(ctx context.Context, tenant uuid.UUID, c SpendCap) error {
	return s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		if c.PerTransaction == nil && c.PerHour == nil && c.PerDay == nil {
			_, err := tx.Exec(ctx, `DELETE FROM spend_caps WHERE tenant_id=$1 AND currency=$2`, tenant, c.Currency)
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO spend_caps (tenant_id, currency, per_transaction_minor, per_hour_minor, per_day_minor, updated_by) VALUES ($1,$2,$3,$4,$5,$6)
			 ON CONFLICT (tenant_id, currency) DO UPDATE SET per_transaction_minor=EXCLUDED.per_transaction_minor,
			   per_hour_minor=EXCLUDED.per_hour_minor, per_day_minor=EXCLUDED.per_day_minor, updated_by=EXCLUDED.updated_by`,
			tenant, c.Currency, c.PerTransaction, c.PerHour, c.PerDay, c.UpdatedBy)
		if code, _ := pgCode(err); code == "23514" {
			return fmt.Errorf("%w: caps must be non-negative and per-transaction <= per-hour <= per-day", ErrConflict)
		}
		return err
	})
}

// checkSpend enforces the caps for an approval that is about to be approved, inside the approving transaction.
// A per-tenant advisory lock, held until commit, serialises concurrent approvals so two of them cannot both slip under
// a cap that only one fits.
func checkSpend(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, kind, currency string, amount *int64) error {
	if (kind != "PAYMENT" && kind != "PURCHASE_ORDER") || amount == nil || currency == "" {
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('venture-spend:' || $1::text, 0))`, tenant.String()); err != nil {
		return err
	}
	var perTx, perHour, perDay *int64
	err := tx.QueryRow(ctx, `SELECT per_transaction_minor, per_hour_minor, per_day_minor FROM spend_caps WHERE tenant_id=$1 AND currency=$2`, tenant, currency).Scan(&perTx, &perHour, &perDay)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if perTx != nil && *amount > *perTx {
		return &SpendCapError{Currency: currency, Dimension: "per transaction", Cap: *perTx, Requested: *amount}
	}
	for _, w := range []struct {
		name  string
		limit *int64
		span  string
	}{{"in the last hour", perHour, "1 hour"}, {"in the last 24 hours", perDay, "24 hours"}} {
		if w.limit == nil {
			continue
		}
		var spent int64
		if err := tx.QueryRow(ctx,
			`SELECT coalesce(sum(amount_minor), 0) FROM approval_queue
			  WHERE tenant_id=$1 AND currency=$2 AND kind IN `+moneyOutKinds+` AND status IN ('APPROVED','EXECUTING','EXECUTED')
			    AND decided_at >= now() - $3::interval`, tenant, currency, w.span).Scan(&spent); err != nil {
			return err
		}
		if spent+*amount > *w.limit {
			return &SpendCapError{Currency: currency, Dimension: w.name, Cap: *w.limit, Spent: spent, Requested: *amount}
		}
	}
	return nil
}

// ───────────────────────────── connections and secrets ─────────────────────────────

// Connection is a business's link to a third-party provider. It holds no credentials.
type Connection struct {
	Provider    string
	Status      string
	Config      map[string]string
	SecretName  string
	ConnectedBy string
	UpdatedAt   time.Time
}

// SaveConnection stores a connection and its sealed secret in one transaction, replacing any earlier connection to the
// same provider.
func (s *PG) SaveConnection(ctx context.Context, tenant uuid.UUID, c Connection, sealed vault.Sealed) error {
	cfg, err := json.Marshal(c.Config)
	if err != nil {
		return err
	}
	if c.Config == nil {
		cfg = []byte("{}")
	}
	return s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO tenant_secrets (tenant_id, name, key_version, nonce, ciphertext) VALUES ($1,$2,$3,$4,$5)
			 ON CONFLICT (tenant_id, name) DO UPDATE SET key_version=EXCLUDED.key_version, nonce=EXCLUDED.nonce, ciphertext=EXCLUDED.ciphertext`,
			tenant, c.SecretName, sealed.KeyVersion, sealed.Nonce, sealed.Ciphertext); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO tenant_connections (tenant_id, provider, status, config, secret_name, connected_by) VALUES ($1,$2,'ACTIVE',$3,$4,$5)
			 ON CONFLICT (tenant_id, provider) DO UPDATE SET status='ACTIVE', config=EXCLUDED.config, secret_name=EXCLUDED.secret_name, connected_by=EXCLUDED.connected_by`,
			tenant, c.Provider, cfg, c.SecretName, c.ConnectedBy)
		return err
	})
}

// Connection returns an ACTIVE connection with its sealed secret, or ErrNotFound.
func (s *PG) Connection(ctx context.Context, tenant uuid.UUID, provider string) (Connection, vault.Sealed, error) {
	var c Connection
	var sealed vault.Sealed
	err := s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		var cfg []byte
		err := tx.QueryRow(ctx,
			`SELECT c.provider, c.status, c.config, c.secret_name, c.connected_by, c.updated_at, s.key_version, s.nonce, s.ciphertext
			   FROM tenant_connections c JOIN tenant_secrets s ON s.tenant_id=c.tenant_id AND s.name=c.secret_name
			  WHERE c.tenant_id=$1 AND c.provider=$2 AND c.status='ACTIVE'`, tenant, provider).
			Scan(&c.Provider, &c.Status, &cfg, &c.SecretName, &c.ConnectedBy, &c.UpdatedAt, &sealed.KeyVersion, &sealed.Nonce, &sealed.Ciphertext)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		c.UpdatedAt = c.UpdatedAt.UTC()
		return json.Unmarshal(cfg, &c.Config)
	})
	return c, sealed, err
}

// Connections lists the tenant's connections without any secret material.
func (s *PG) Connections(ctx context.Context, tenant uuid.UUID) ([]Connection, error) {
	var out []Connection
	err := s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT provider, status, config, secret_name, connected_by, updated_at FROM tenant_connections WHERE tenant_id=$1 ORDER BY provider`, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c Connection
			var cfg []byte
			if err := rows.Scan(&c.Provider, &c.Status, &cfg, &c.SecretName, &c.ConnectedBy, &c.UpdatedAt); err != nil {
				return err
			}
			c.UpdatedAt = c.UpdatedAt.UTC()
			if err := json.Unmarshal(cfg, &c.Config); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// DeleteConnection removes a connection and destroys its sealed secret.
func (s *PG) DeleteConnection(ctx context.Context, tenant uuid.UUID, provider string) error {
	return s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		var name string
		err := tx.QueryRow(ctx, `DELETE FROM tenant_connections WHERE tenant_id=$1 AND provider=$2 RETURNING secret_name`, tenant, provider).Scan(&name)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM tenant_secrets WHERE tenant_id=$1 AND name=$2`, tenant, name)
		return err
	})
}

// ───────────────────────────── audit verification ─────────────────────────────

// AuditVerdict is the outcome of walking a tenant's audit chain.
type AuditVerdict struct {
	Intact      bool
	FirstBadID  int64 // set when !Intact
	RowsChecked int64
	Head        []byte // the newest hash: anchor it outside the database to detect truncation
}

// VerifyAudit walks the tenant's audit hash chain.
func (s *PG) VerifyAudit(ctx context.Context, tenant uuid.UUID) (AuditVerdict, error) {
	var v AuditVerdict
	err := s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		var bad *int64
		if err := tx.QueryRow(ctx, `SELECT first_bad_id, rows_checked, head FROM audit_verify($1)`, tenant).Scan(&bad, &v.RowsChecked, &v.Head); err != nil {
			return err
		}
		v.Intact = bad == nil
		if bad != nil {
			v.FirstBadID = *bad
		}
		return nil
	})
	return v, err
}

// ───────────────────────────── idempotent external calls ─────────────────────────────

// IdemState says what an idempotency key's earlier history is.
type IdemState int

const (
	// IdemNew: the key was free and is now held by this caller, who must finish with CompleteIdem or AbandonIdem.
	IdemNew IdemState = iota
	// IdemDone: an earlier call with the same request finished; its recorded response is returned.
	IdemDone
	// IdemUnknown: an earlier call with the same request started and never recorded an outcome (it may have reached the
	// provider). Repeating it could duplicate the effect, so the caller must not.
	IdemUnknown
)

// BeginIdem claims an idempotency key for an external call (creating a Zoom meeting, a Notion page), which the
// provider cannot de-duplicate itself. Reusing a key with a different request is ErrIdempotencyMismatch. Keys expire
// after 48 hours.
func (s *PG) BeginIdem(ctx context.Context, tenant uuid.UUID, key string, requestHash [32]byte) (IdemState, json.RawMessage, error) {
	state := IdemNew
	var resp json.RawMessage
	err := s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE tenant_id=$1 AND key=$2 AND expires_at < now()`, tenant, key); err != nil {
			return err
		}
		ct, err := tx.Exec(ctx, `INSERT INTO idempotency_keys (tenant_id, key, request_hash) VALUES ($1,$2,$3) ON CONFLICT (tenant_id, key) DO NOTHING`, tenant, key, requestHash[:])
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 1 {
			return nil
		}
		var stored []byte
		var status string
		if err := tx.QueryRow(ctx, `SELECT request_hash, status, response FROM idempotency_keys WHERE tenant_id=$1 AND key=$2`, tenant, key).Scan(&stored, &status, &resp); err != nil {
			return err
		}
		if !bytes.Equal(stored, requestHash[:]) {
			return ErrIdempotencyMismatch
		}
		if status == "COMPLETED" {
			state = IdemDone
		} else {
			state = IdemUnknown
		}
		return nil
	})
	return state, resp, err
}

// CompleteIdem records the response of a call that finished, so a repeat returns it instead of acting again.
func (s *PG) CompleteIdem(ctx context.Context, tenant uuid.UUID, key string, response json.RawMessage) error {
	return s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE idempotency_keys SET status='COMPLETED', response=$3 WHERE tenant_id=$1 AND key=$2`, tenant, key, []byte(response))
		return err
	})
}

// AbandonIdem releases a key whose call certainly did NOT reach the provider (it was rejected before sending), so the
// caller may retry.
func (s *PG) AbandonIdem(ctx context.Context, tenant uuid.UUID, key string) error {
	return s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE tenant_id=$1 AND key=$2 AND status='IN_PROGRESS'`, tenant, key)
		return err
	})
}

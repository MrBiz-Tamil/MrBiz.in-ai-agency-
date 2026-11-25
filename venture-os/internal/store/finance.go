package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mrbiz/venture-os/internal/financeengine"
)

// ApprovedBlueprint returns the tenant's live (APPROVED) blueprint.
func (s *PG) ApprovedBlueprint(ctx context.Context, tenant uuid.UUID) (StoredBlueprint, error) {
	var sb StoredBlueprint
	err := s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM company_blueprints WHERE tenant_id=$1 AND status='APPROVED'`, tenant).Scan(&id); err != nil {
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

// PricingRules returns the tenant's ACTIVE pricing rules ordered by name.
func (s *PG) PricingRules(ctx context.Context, tenant uuid.UUID) ([]financeengine.Rule, error) {
	var out []financeengine.Rule
	err := s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT name, model, currency, formula, margin_floor_bps, margin_target_bps, approval_threshold_minor
			 FROM tenant_pricing_rules WHERE tenant_id=$1 AND status='ACTIVE' ORDER BY name`, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r financeengine.Rule
			var model string
			var formula []byte
			if err := rows.Scan(&r.Name, &model, &r.Currency, &formula, &r.MarginFloorBps, &r.MarginTargetBps, &r.ApprovalThresholdMinor); err != nil {
				return err
			}
			r.Model = pricingModel(model)
			r.Formula = json.RawMessage(formula)
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// LedgerEntry is one line of a journal to post. Exactly one of Debit/Credit is positive.
type LedgerEntry struct {
	AccountCode string
	Debit       int64
	Credit      int64
	Currency    string
}

// Journal is a balanced set of entries.
type Journal struct {
	Memo           string
	SourceType     string
	SourceID       string
	CreatedBy      string
	IdempotencyKey string
	EffectiveDate  time.Time
	Entries        []LedgerEntry
}

// PostJournal posts a journal atomically. Replaying the idempotency key returns the original journal id with
// created=false. The database rejects unbalanced journals, unknown/inactive accounts and wrong currencies.
func (s *PG) PostJournal(ctx context.Context, tenant uuid.UUID, j Journal) (id uuid.UUID, created bool, err error) {
	if len(j.Entries) < 2 {
		return uuid.Nil, false, fmt.Errorf("a journal needs at least two entries")
	}
	err = s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		switch e := tx.QueryRow(ctx, `SELECT id FROM ledger_journals WHERE tenant_id=$1 AND idempotency_key=$2`, tenant, j.IdempotencyKey).Scan(&id); {
		case e == nil:
			created = false
			return nil
		case !errors.Is(e, pgx.ErrNoRows):
			return e
		}
		date := j.EffectiveDate
		if date.IsZero() {
			date = time.Now().UTC()
		}
		if e := tx.QueryRow(ctx,
			`INSERT INTO ledger_journals (tenant_id, entry_count, effective_date, memo, source_type, source_id, created_by, idempotency_key)
			 VALUES ($1,$2,$3,$4,nullif($5,''),nullif($6,''),$7,$8) RETURNING id`,
			tenant, len(j.Entries), date, j.Memo, j.SourceType, j.SourceID, j.CreatedBy, j.IdempotencyKey).Scan(&id); e != nil {
			return e
		}
		for i, e := range j.Entries {
			ct, err := tx.Exec(ctx,
				`INSERT INTO ledger_entries (tenant_id, journal_id, line_no, account_id, debit_minor, credit_minor, currency)
				 SELECT $1,$2,$3,id,$5,$6,$7 FROM ledger_accounts WHERE tenant_id=$1 AND code=$4`,
				tenant, id, i+1, e.AccountCode, e.Debit, e.Credit, e.Currency)
			if err != nil {
				return err
			}
			if ct.RowsAffected() != 1 {
				return fmt.Errorf("%w: ledger account %q", ErrNotFound, e.AccountCode)
			}
		}
		created = true
		return nil
	})
	return id, created, err
}

// AccountBalance returns debits minus credits posted to the account with the given code (0 if none).
func (s *PG) AccountBalance(ctx context.Context, tenant uuid.UUID, code string) (int64, error) {
	var bal int64
	err := s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT coalesce(sum(e.debit_minor - e.credit_minor), 0)::bigint
			 FROM ledger_entries e JOIN ledger_accounts a ON a.tenant_id=e.tenant_id AND a.id=e.account_id
			 WHERE e.tenant_id=$1 AND a.code=$2`, tenant, code).Scan(&bal)
	})
	return bal, err
}

// CashflowItems returns scheduled (FORECAST/COMMITTED) movements due on or before `until`, including overdue ones.
func (s *PG) CashflowItems(ctx context.Context, tenant uuid.UUID, until time.Time) ([]financeengine.Item, error) {
	var out []financeengine.Item
	err := s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT direction, amount_minor, due_date, status, category FROM cashflow_items
			 WHERE tenant_id=$1 AND status IN ('FORECAST','COMMITTED') AND due_date <= $2 ORDER BY due_date, id`, tenant, until)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it financeengine.Item
			var dir string
			if err := rows.Scan(&dir, &it.AmountMinor, &it.Due, &it.Status, &it.Category); err != nil {
				return err
			}
			it.Direction = financeengine.Direction(dir)
			out = append(out, it)
		}
		return rows.Err()
	})
	return out, err
}

// CashflowItem is a scheduled cash movement to record.
type CashflowItem struct {
	Direction      string // INFLOW | OUTFLOW
	Category       string
	AmountMinor    int64
	Currency       string
	Due            time.Time
	Status         string // FORECAST | COMMITTED
	SourceType     string
	SourceID       string
	Memo           string
	IdempotencyKey string
}

// AddCashflowItem records a scheduled movement. Replaying the key is a no-op (created=false).
func (s *PG) AddCashflowItem(ctx context.Context, tenant uuid.UUID, it CashflowItem) (created bool, err error) {
	err = s.tenantTx(ctx, tenant, func(tx pgx.Tx) error {
		ct, e := tx.Exec(ctx,
			`INSERT INTO cashflow_items (tenant_id, direction, category, amount_minor, currency, due_date, status, source_type, source_id, memo, idempotency_key)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,nullif($8,''),nullif($9,''),$10,$11)
			 ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`,
			tenant, it.Direction, it.Category, it.AmountMinor, it.Currency, it.Due, it.Status, it.SourceType, it.SourceID, it.Memo, it.IdempotencyKey)
		created = e == nil && ct.RowsAffected() == 1
		return e
	})
	return created, err
}

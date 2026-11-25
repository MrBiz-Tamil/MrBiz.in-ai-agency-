package builtin

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mrbiz/venture-os/internal/crm"
)

// ───────────────────────────── deals ─────────────────────────────

const dealCols = `id::text, name, company_id::text, coalesce(contact_id::text, ''), pipeline_key, stage, amount_minor, currency,
	expected_close, owner_sub, fields, stage_changed_at, version, created_at, updated_at`

type dealRow struct {
	d          crm.Deal
	id, co, ct string
	amount     *int64
	currency   *string
	close      *time.Time
}

func (r *dealRow) dest() []any {
	return []any{&r.id, &r.d.Name, &r.co, &r.ct, &r.d.Pipeline, &r.d.Stage, &r.amount, &r.currency, &r.close,
		&r.d.OwnerSub, &r.d.Fields, &r.d.StageChangedAt, &r.d.Version, &r.d.CreatedAt, &r.d.UpdatedAt}
}

func (r *dealRow) done() crm.Deal {
	r.d.ID, r.d.CompanyID, r.d.ContactID = crm.ID(r.id), crm.ID(r.co), crm.ID(r.ct)
	if r.amount != nil && r.currency != nil {
		r.d.Amount = &crm.Money{Minor: *r.amount, Currency: *r.currency}
	}
	r.d.ExpectedClose = crm.Date(utc(r.close))
	r.d.StageChangedAt, r.d.CreatedAt, r.d.UpdatedAt = r.d.StageChangedAt.UTC(), r.d.CreatedAt.UTC(), r.d.UpdatedAt.UTC()
	return r.d
}

func getDeal(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID, includeArchived, lock bool) (crm.Deal, error) {
	r := &dealRow{}
	if err := fetch(ctx, tx, "crm_deals", dealCols, true, tenant, id, includeArchived, lock, r.dest()...); err != nil {
		return crm.Deal{}, err
	}
	return r.done(), nil
}

// moneyArgs splits an optional amount into the two nullable columns.
func moneyArgs(m *crm.Money) (minor, currency any) {
	if m == nil {
		return nil, nil
	}
	return m.Minor, m.Currency
}

func dateArg(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func (c *client) CreateDeal(ctx context.Context, key string, in crm.DealInput) (crm.Deal, bool, error) {
	if err := crm.ValidateKey(key); err != nil {
		return crm.Deal{}, false, err
	}
	var out crm.Deal
	var created bool
	err := c.tx(ctx, func(tx pgx.Tx) error {
		pipes, err := loadPipelines(ctx, tx, c.tenant)
		if err != nil {
			return err
		}
		norm, _, err := crm.NormalizeDeal(in, pipes)
		if err != nil {
			return err
		}
		hash := crm.HashInput(norm)
		id, err := replayID(ctx, tx, "crm_deals", "deal", c.tenant, key, hash)
		if err != nil {
			return err
		}
		if id == "" {
			if err := requireRef(ctx, tx, "crm_companies", "company", c.tenant, norm.CompanyID); err != nil {
				return err
			}
			company, _ := parseID(norm.CompanyID)
			var contact any
			if norm.ContactID != "" {
				if err := requireRef(ctx, tx, "crm_contacts", "contact", c.tenant, norm.ContactID); err != nil {
					return err
				}
				contact, _ = optUUID(norm.ContactID)
			}
			minor, cur := moneyArgs(norm.Amount)
			err = tx.QueryRow(ctx,
				`INSERT INTO crm_deals (tenant_id, name, company_id, contact_id, pipeline_key, stage, amount_minor, currency,
				                        expected_close, owner_sub, fields, idempotency_key, request_hash)
				 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
				 ON CONFLICT (tenant_id, idempotency_key) DO NOTHING RETURNING id::text`,
				c.tenant, norm.Name, company, contact, norm.Pipeline, norm.Stage, minor, cur,
				dateArg(norm.ExpectedClose), norm.OwnerSub, norm.Fields, key, hash[:]).Scan(&id)
			switch {
			case err == nil:
				created = true
				u, _ := parseID(crm.ID(id))
				if _, err := tx.Exec(ctx,
					`INSERT INTO crm_deal_stage_history (tenant_id, deal_id, from_stage, to_stage, changed_by) VALUES ($1,$2,'',$3,$4)`,
					c.tenant, u, norm.Stage, norm.CreatedBy); err != nil {
					return err
				}
			case errors.Is(err, pgx.ErrNoRows):
				if id, err = replayID(ctx, tx, "crm_deals", "deal", c.tenant, key, hash); err != nil || id == "" {
					return orMissing(err)
				}
			default:
				return err
			}
		}
		u, _ := parseID(crm.ID(id))
		out, err = getDeal(ctx, tx, c.tenant, u, true, false)
		return err
	})
	return out, created && err == nil, err
}

func (c *client) GetDeal(ctx context.Context, id crm.ID) (crm.Deal, error) {
	u, ok := parseID(id)
	if !ok {
		return crm.Deal{}, notFound("deal", id)
	}
	var out crm.Deal
	err := c.tx(ctx, func(tx pgx.Tx) (err error) {
		out, err = getDeal(ctx, tx, c.tenant, u, false, false)
		return err
	})
	return out, mapNotFound(err, "deal", id)
}

func (c *client) UpdateDeal(ctx context.Context, id crm.ID, p crm.DealPatch, ifVersion int64) (crm.Deal, error) {
	u, ok := parseID(id)
	if !ok {
		return crm.Deal{}, notFound("deal", id)
	}
	var out crm.Deal
	err := c.tx(ctx, func(tx pgx.Tx) error {
		cur, err := getDeal(ctx, tx, c.tenant, u, false, true)
		if err != nil {
			return err
		}
		pipes, err := loadPipelines(ctx, tx, c.tenant)
		if err != nil {
			return err
		}
		pipe, found := crm.FindPipeline(pipes, cur.Pipeline)
		if !found {
			return invalidRef("the deal's pipeline %q is no longer active", cur.Pipeline)
		}
		if p, err = crm.NormalizeDealPatch(p, pipe); err != nil {
			return err
		}
		if err := checkVersion(ifVersion, cur.Version); err != nil {
			return err
		}
		set(&cur.Name, p.Name)
		set(&cur.OwnerSub, p.OwnerSub)
		if p.ContactID != nil {
			if *p.ContactID != "" {
				if err := requireRef(ctx, tx, "crm_contacts", "contact", c.tenant, *p.ContactID); err != nil {
					return err
				}
			}
			cur.ContactID = *p.ContactID
		}
		switch {
		case p.ClearAmount:
			cur.Amount = nil
		case p.Amount != nil:
			a := *p.Amount
			cur.Amount = &a
		}
		switch {
		case p.ClearExpectedClose:
			cur.ExpectedClose = time.Time{}
		case p.ExpectedClose != nil:
			cur.ExpectedClose = *p.ExpectedClose
		}
		if cur.Fields, err = crm.MergeFields(cur.Fields, p.Fields); err != nil {
			return err
		}
		contact, err := optUUID(cur.ContactID)
		if err != nil {
			return err
		}
		minor, currency := moneyArgs(cur.Amount)
		r := &dealRow{}
		if err := tx.QueryRow(ctx,
			`UPDATE crm_deals SET name=$3, contact_id=$4, amount_minor=$5, currency=$6, expected_close=$7, owner_sub=$8, fields=$9, version=version+1
			  WHERE tenant_id=$1 AND id=$2 RETURNING `+dealCols,
			c.tenant, u, cur.Name, contact, minor, currency, dateArg(cur.ExpectedClose), cur.OwnerSub, cur.Fields).Scan(r.dest()...); err != nil {
			return err
		}
		out = r.done()
		return nil
	})
	return out, mapNotFound(err, "deal", id)
}

func (c *client) MoveDeal(ctx context.Context, id crm.ID, toStage, by string, ifVersion int64) (crm.Deal, error) {
	u, ok := parseID(id)
	if !ok {
		return crm.Deal{}, notFound("deal", id)
	}
	var out crm.Deal
	err := c.tx(ctx, func(tx pgx.Tx) error {
		cur, err := getDeal(ctx, tx, c.tenant, u, false, true)
		if err != nil {
			return err
		}
		pipes, err := loadPipelines(ctx, tx, c.tenant)
		if err != nil {
			return err
		}
		pipe, found := crm.FindPipeline(pipes, cur.Pipeline)
		if !found || !crm.HasStage(pipe, toStage) {
			return invalidRef("pipeline %q has no stage %q", cur.Pipeline, toStage)
		}
		if utf8.RuneCountInString(by) > 200 {
			return invalidRef("changed-by is longer than 200 characters")
		}
		if err := checkVersion(ifVersion, cur.Version); err != nil {
			return err
		}
		if cur.Stage == toStage {
			out = cur // no-op: no version bump, no history entry
			return nil
		}
		r := &dealRow{}
		if err := tx.QueryRow(ctx,
			`UPDATE crm_deals SET stage=$3, stage_changed_at=now(), version=version+1 WHERE tenant_id=$1 AND id=$2 RETURNING `+dealCols,
			c.tenant, u, toStage).Scan(r.dest()...); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO crm_deal_stage_history (tenant_id, deal_id, from_stage, to_stage, changed_by) VALUES ($1,$2,$3,$4,$5)`,
			c.tenant, u, cur.Stage, toStage, by); err != nil {
			return err
		}
		out = r.done()
		return nil
	})
	return out, mapNotFound(err, "deal", id)
}

func (c *client) ArchiveDeal(ctx context.Context, id crm.ID, ifVersion int64) error {
	return c.archive(ctx, "crm_deals", "deal", id, ifVersion)
}

func (c *client) ListDeals(ctx context.Context, f crm.DealFilter, p crm.PageRequest) (crm.Page[crm.Deal], error) {
	size, after, err := pageArgs(p)
	if err != nil {
		return crm.Page[crm.Deal]{}, err
	}
	var out crm.Page[crm.Deal]
	err = c.tx(ctx, func(tx pgx.Tx) (err error) {
		q := newList("crm_deals", dealCols, c.tenant, after, !f.IncludeArchived)
		if !q.idEq("company_id", f.CompanyID) {
			return nil
		}
		q.search(f.Query, "name")
		q.textEq("pipeline_key", f.Pipeline)
		q.textEq("stage", f.Stage)
		q.textEq("owner_sub", f.OwnerSub)
		out, err = runList(ctx, tx, q, size, func() ([]any, func() crm.Deal) { r := &dealRow{}; return r.dest(), r.done })
		return err
	})
	return out, err
}

func (c *client) DealHistory(ctx context.Context, id crm.ID) ([]crm.StageChange, error) {
	u, ok := parseID(id)
	if !ok {
		return nil, notFound("deal", id)
	}
	var out []crm.StageChange
	err := c.tx(ctx, func(tx pgx.Tx) error {
		var one int
		if err := fetch(ctx, tx, "crm_deals", "1", true, c.tenant, u, false, false, &one); err != nil {
			return err
		}
		rows, err := tx.Query(ctx,
			`SELECT from_stage, to_stage, changed_by, changed_at FROM crm_deal_stage_history
			  WHERE tenant_id = $1 AND deal_id = $2 ORDER BY seq`, c.tenant, u)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = []crm.StageChange{}
		for rows.Next() {
			var s crm.StageChange
			if err := rows.Scan(&s.From, &s.To, &s.ChangedBy, &s.ChangedAt); err != nil {
				return err
			}
			s.ChangedAt = s.ChangedAt.UTC()
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, mapNotFound(err, "deal", id)
}

// ───────────────────────────── activities ─────────────────────────────

const activityCols = `id::text, kind, subject, body, due_at, completed_at, coalesce(company_id::text, ''), coalesce(contact_id::text, ''),
	coalesce(deal_id::text, ''), owner_sub, created_by, created_at`

type activityRow struct {
	a          crm.Activity
	id         string
	co, ct, dl string
	kind       string
	due, done_ *time.Time
}

func (r *activityRow) dest() []any {
	return []any{&r.id, &r.kind, &r.a.Subject, &r.a.Body, &r.due, &r.done_, &r.co, &r.ct, &r.dl, &r.a.OwnerSub, &r.a.CreatedBy, &r.a.CreatedAt}
}

func (r *activityRow) done() crm.Activity {
	r.a.ID, r.a.Kind = crm.ID(r.id), crm.ActivityKind(r.kind)
	r.a.CompanyID, r.a.ContactID, r.a.DealID = crm.ID(r.co), crm.ID(r.ct), crm.ID(r.dl)
	r.a.DueAt, r.a.CompletedAt, r.a.CreatedAt = utc(r.due), utc(r.done_), r.a.CreatedAt.UTC()
	return r.a
}

func getActivity(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID, lock bool) (crm.Activity, error) {
	r := &activityRow{}
	if err := fetch(ctx, tx, "crm_activities", activityCols, false, tenant, id, true, lock, r.dest()...); err != nil {
		return crm.Activity{}, err
	}
	return r.done(), nil
}

func (c *client) CreateActivity(ctx context.Context, key string, in crm.ActivityInput) (crm.Activity, bool, error) {
	if err := crm.ValidateKey(key); err != nil {
		return crm.Activity{}, false, err
	}
	norm, err := crm.NormalizeActivity(in)
	if err != nil {
		return crm.Activity{}, false, err
	}
	hash := crm.HashInput(norm)
	var out crm.Activity
	var created bool
	err = c.tx(ctx, func(tx pgx.Tx) error {
		id, err := replayID(ctx, tx, "crm_activities", "activity", c.tenant, key, hash)
		if err != nil {
			return err
		}
		if id == "" {
			var args [3]any
			for i, ref := range []struct {
				table, what string
				id          crm.ID
			}{{"crm_companies", "company", norm.CompanyID}, {"crm_contacts", "contact", norm.ContactID}, {"crm_deals", "deal", norm.DealID}} {
				if ref.id == "" {
					continue
				}
				if err := requireRef(ctx, tx, ref.table, ref.what, c.tenant, ref.id); err != nil {
					return err
				}
				args[i], _ = optUUID(ref.id)
			}
			err = tx.QueryRow(ctx,
				`INSERT INTO crm_activities (tenant_id, kind, subject, body, due_at, company_id, contact_id, deal_id, owner_sub, created_by, idempotency_key, request_hash)
				 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
				 ON CONFLICT (tenant_id, idempotency_key) DO NOTHING RETURNING id::text`,
				c.tenant, string(norm.Kind), norm.Subject, norm.Body, dateArg(norm.DueAt), args[0], args[1], args[2],
				norm.OwnerSub, norm.CreatedBy, key, hash[:]).Scan(&id)
			switch {
			case err == nil:
				created = true
			case errors.Is(err, pgx.ErrNoRows):
				if id, err = replayID(ctx, tx, "crm_activities", "activity", c.tenant, key, hash); err != nil || id == "" {
					return orMissing(err)
				}
			default:
				return err
			}
		}
		u, _ := parseID(crm.ID(id))
		out, err = getActivity(ctx, tx, c.tenant, u, false)
		return err
	})
	return out, created && err == nil, err
}

func (c *client) GetActivity(ctx context.Context, id crm.ID) (crm.Activity, error) {
	u, ok := parseID(id)
	if !ok {
		return crm.Activity{}, notFound("activity", id)
	}
	var out crm.Activity
	err := c.tx(ctx, func(tx pgx.Tx) (err error) {
		out, err = getActivity(ctx, tx, c.tenant, u, false)
		return err
	})
	return out, mapNotFound(err, "activity", id)
}

func (c *client) CompleteActivity(ctx context.Context, id crm.ID, by string) (crm.Activity, error) {
	u, ok := parseID(id)
	if !ok {
		return crm.Activity{}, notFound("activity", id)
	}
	if utf8.RuneCountInString(by) > 200 {
		return crm.Activity{}, invalidRef("completed-by is longer than 200 characters")
	}
	var out crm.Activity
	err := c.tx(ctx, func(tx pgx.Tx) error {
		cur, err := getActivity(ctx, tx, c.tenant, u, true)
		if err != nil {
			return err
		}
		if cur.Kind != crm.ActivityTask {
			return invalidRef("only a TASK can be completed, this is a %s", cur.Kind)
		}
		if !cur.CompletedAt.IsZero() {
			out = cur
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE crm_activities SET completed_at = now(), completed_by = $3 WHERE tenant_id = $1 AND id = $2`, c.tenant, u, by); err != nil {
			return err
		}
		out, err = getActivity(ctx, tx, c.tenant, u, false)
		return err
	})
	return out, mapNotFound(err, "activity", id)
}

func (c *client) ListActivities(ctx context.Context, f crm.ActivityFilter, p crm.PageRequest) (crm.Page[crm.Activity], error) {
	size, after, err := pageArgs(p)
	if err != nil {
		return crm.Page[crm.Activity]{}, err
	}
	var out crm.Page[crm.Activity]
	err = c.tx(ctx, func(tx pgx.Tx) (err error) {
		q := newList("crm_activities", activityCols, c.tenant, after, false)
		if !q.idEq("company_id", f.CompanyID) || !q.idEq("contact_id", f.ContactID) || !q.idEq("deal_id", f.DealID) {
			return nil
		}
		q.textEq("kind", string(f.Kind))
		q.textEq("owner_sub", f.OwnerSub)
		if f.OpenTasksOnly {
			q.and("kind = 'TASK' AND completed_at IS NULL")
		}
		out, err = runList(ctx, tx, q, size, func() ([]any, func() crm.Activity) { r := &activityRow{}; return r.dest(), r.done })
		return err
	})
	return out, err
}

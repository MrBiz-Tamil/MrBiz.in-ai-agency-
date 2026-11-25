package builtin

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mrbiz/venture-os/internal/crm"
)

// ───────────────────────────── companies ─────────────────────────────

const companyCols = `id::text, name, domain, industry, country, website, phone, owner_sub, fields, version, created_at, updated_at`

type companyRow struct {
	c  crm.Company
	id string
}

func (r *companyRow) dest() []any {
	return []any{&r.id, &r.c.Name, &r.c.Domain, &r.c.Industry, &r.c.Country, &r.c.Website, &r.c.Phone, &r.c.OwnerSub,
		&r.c.Fields, &r.c.Version, &r.c.CreatedAt, &r.c.UpdatedAt}
}

func (r *companyRow) done() crm.Company {
	r.c.ID = crm.ID(r.id)
	r.c.CreatedAt, r.c.UpdatedAt = r.c.CreatedAt.UTC(), r.c.UpdatedAt.UTC()
	return r.c
}

func getCompany(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID, includeArchived, lock bool) (crm.Company, error) {
	r := &companyRow{}
	if err := fetch(ctx, tx, "crm_companies", companyCols, true, tenant, id, includeArchived, lock, r.dest()...); err != nil {
		return crm.Company{}, err
	}
	return r.done(), nil
}

func (c *client) CreateCompany(ctx context.Context, key string, in crm.CompanyInput) (crm.Company, bool, error) {
	if err := crm.ValidateKey(key); err != nil {
		return crm.Company{}, false, err
	}
	norm, err := crm.NormalizeCompany(in)
	if err != nil {
		return crm.Company{}, false, err
	}
	hash := crm.HashInput(norm)
	var out crm.Company
	var created bool
	err = c.tx(ctx, func(tx pgx.Tx) error {
		id, err := replayID(ctx, tx, "crm_companies", "company", c.tenant, key, hash)
		if err != nil {
			return err
		}
		if id == "" {
			err = tx.QueryRow(ctx,
				`INSERT INTO crm_companies (tenant_id, name, domain, industry, country, website, phone, owner_sub, fields, idempotency_key, request_hash)
				 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
				 ON CONFLICT (tenant_id, idempotency_key) DO NOTHING RETURNING id::text`,
				c.tenant, norm.Name, norm.Domain, norm.Industry, norm.Country, norm.Website, norm.Phone, norm.OwnerSub, norm.Fields, key, hash[:]).Scan(&id)
			switch {
			case err == nil:
				created = true
			case err == pgx.ErrNoRows: // a concurrent request with the same key won the insert
				if id, err = replayID(ctx, tx, "crm_companies", "company", c.tenant, key, hash); err != nil || id == "" {
					return orMissing(err)
				}
			default:
				return err
			}
		}
		u, _ := parseID(crm.ID(id))
		out, err = getCompany(ctx, tx, c.tenant, u, true, false)
		return err
	})
	return out, created && err == nil, err
}

func (c *client) GetCompany(ctx context.Context, id crm.ID) (crm.Company, error) {
	u, ok := parseID(id)
	if !ok {
		return crm.Company{}, notFound("company", id)
	}
	var out crm.Company
	err := c.tx(ctx, func(tx pgx.Tx) (err error) {
		out, err = getCompany(ctx, tx, c.tenant, u, false, false)
		return err
	})
	return out, mapNotFound(err, "company", id)
}

func (c *client) UpdateCompany(ctx context.Context, id crm.ID, p crm.CompanyPatch, ifVersion int64) (crm.Company, error) {
	p, err := crm.NormalizeCompanyPatch(p)
	if err != nil {
		return crm.Company{}, err
	}
	u, ok := parseID(id)
	if !ok {
		return crm.Company{}, notFound("company", id)
	}
	var out crm.Company
	err = c.tx(ctx, func(tx pgx.Tx) error {
		cur, err := getCompany(ctx, tx, c.tenant, u, false, true)
		if err != nil {
			return err
		}
		if err := checkVersion(ifVersion, cur.Version); err != nil {
			return err
		}
		set(&cur.Name, p.Name)
		set(&cur.Domain, p.Domain)
		set(&cur.Industry, p.Industry)
		set(&cur.Country, p.Country)
		set(&cur.Website, p.Website)
		set(&cur.Phone, p.Phone)
		set(&cur.OwnerSub, p.OwnerSub)
		if cur.Fields, err = crm.MergeFields(cur.Fields, p.Fields); err != nil {
			return err
		}
		r := &companyRow{}
		if err := tx.QueryRow(ctx,
			`UPDATE crm_companies SET name=$3, domain=$4, industry=$5, country=$6, website=$7, phone=$8, owner_sub=$9, fields=$10, version=version+1
			  WHERE tenant_id=$1 AND id=$2 RETURNING `+companyCols,
			c.tenant, u, cur.Name, cur.Domain, cur.Industry, cur.Country, cur.Website, cur.Phone, cur.OwnerSub, cur.Fields).Scan(r.dest()...); err != nil {
			return err
		}
		out = r.done()
		return nil
	})
	return out, mapNotFound(err, "company", id)
}

func (c *client) ArchiveCompany(ctx context.Context, id crm.ID, ifVersion int64) error {
	return c.archive(ctx, "crm_companies", "company", id, ifVersion)
}

// archive soft-deletes a row (companies, contacts, deals share the shape), under the same version rules as updates.
func (c *client) archive(ctx context.Context, table, kind string, id crm.ID, ifVersion int64) error {
	u, ok := parseID(id)
	if !ok {
		return notFound(kind, id)
	}
	err := c.tx(ctx, func(tx pgx.Tx) error {
		var version int64
		if err := fetch(ctx, tx, table, "version", true, c.tenant, u, false, true, &version); err != nil {
			return err
		}
		if err := checkVersion(ifVersion, version); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE `+table+` SET archived_at = now(), version = version + 1 WHERE tenant_id = $1 AND id = $2`, c.tenant, u)
		return err
	})
	return mapNotFound(err, kind, id)
}

func (c *client) ListCompanies(ctx context.Context, f crm.CompanyFilter, p crm.PageRequest) (crm.Page[crm.Company], error) {
	size, after, err := pageArgs(p)
	if err != nil {
		return crm.Page[crm.Company]{}, err
	}
	var out crm.Page[crm.Company]
	err = c.tx(ctx, func(tx pgx.Tx) (err error) {
		q := newList("crm_companies", companyCols, c.tenant, after, !f.IncludeArchived)
		q.search(f.Query, "name", "domain")
		q.textEq("owner_sub", f.OwnerSub)
		out, err = runList(ctx, tx, q, size, func() ([]any, func() crm.Company) { r := &companyRow{}; return r.dest(), r.done })
		return err
	})
	return out, err
}

// ───────────────────────────── contacts ─────────────────────────────

const contactCols = `id::text, coalesce(company_id::text, ''), first_name, last_name, email, phone, title, owner_sub, fields, version, created_at, updated_at`

type contactRow struct {
	c  crm.Contact
	id string
	co string
}

func (r *contactRow) dest() []any {
	return []any{&r.id, &r.co, &r.c.FirstName, &r.c.LastName, &r.c.Email, &r.c.Phone, &r.c.Title, &r.c.OwnerSub,
		&r.c.Fields, &r.c.Version, &r.c.CreatedAt, &r.c.UpdatedAt}
}

func (r *contactRow) done() crm.Contact {
	r.c.ID, r.c.CompanyID = crm.ID(r.id), crm.ID(r.co)
	r.c.CreatedAt, r.c.UpdatedAt = r.c.CreatedAt.UTC(), r.c.UpdatedAt.UTC()
	return r.c
}

func getContact(ctx context.Context, tx pgx.Tx, tenant, id uuid.UUID, includeArchived, lock bool) (crm.Contact, error) {
	r := &contactRow{}
	if err := fetch(ctx, tx, "crm_contacts", contactCols, true, tenant, id, includeArchived, lock, r.dest()...); err != nil {
		return crm.Contact{}, err
	}
	return r.done(), nil
}

func (c *client) CreateContact(ctx context.Context, key string, in crm.ContactInput) (crm.Contact, bool, error) {
	if err := crm.ValidateKey(key); err != nil {
		return crm.Contact{}, false, err
	}
	norm, err := crm.NormalizeContact(in)
	if err != nil {
		return crm.Contact{}, false, err
	}
	hash := crm.HashInput(norm)
	var out crm.Contact
	var created bool
	err = c.tx(ctx, func(tx pgx.Tx) error {
		id, err := replayID(ctx, tx, "crm_contacts", "contact", c.tenant, key, hash)
		if err != nil {
			return err
		}
		if id == "" {
			var company any
			if norm.CompanyID != "" {
				if err := requireRef(ctx, tx, "crm_companies", "company", c.tenant, norm.CompanyID); err != nil {
					return err
				}
				u, _ := parseID(norm.CompanyID)
				company = u
			}
			err = tx.QueryRow(ctx,
				`INSERT INTO crm_contacts (tenant_id, company_id, first_name, last_name, email, phone, title, owner_sub, fields, idempotency_key, request_hash)
				 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
				 ON CONFLICT (tenant_id, idempotency_key) DO NOTHING RETURNING id::text`,
				c.tenant, company, norm.FirstName, norm.LastName, norm.Email, norm.Phone, norm.Title, norm.OwnerSub, norm.Fields, key, hash[:]).Scan(&id)
			switch {
			case err == nil:
				created = true
			case err == pgx.ErrNoRows:
				if id, err = replayID(ctx, tx, "crm_contacts", "contact", c.tenant, key, hash); err != nil || id == "" {
					return orMissing(err)
				}
			default:
				return err
			}
		}
		u, _ := parseID(crm.ID(id))
		out, err = getContact(ctx, tx, c.tenant, u, true, false)
		return err
	})
	return out, created && err == nil, err
}

func (c *client) GetContact(ctx context.Context, id crm.ID) (crm.Contact, error) {
	u, ok := parseID(id)
	if !ok {
		return crm.Contact{}, notFound("contact", id)
	}
	var out crm.Contact
	err := c.tx(ctx, func(tx pgx.Tx) (err error) {
		out, err = getContact(ctx, tx, c.tenant, u, false, false)
		return err
	})
	return out, mapNotFound(err, "contact", id)
}

func (c *client) UpdateContact(ctx context.Context, id crm.ID, p crm.ContactPatch, ifVersion int64) (crm.Contact, error) {
	p, err := crm.NormalizeContactPatch(p)
	if err != nil {
		return crm.Contact{}, err
	}
	u, ok := parseID(id)
	if !ok {
		return crm.Contact{}, notFound("contact", id)
	}
	var out crm.Contact
	err = c.tx(ctx, func(tx pgx.Tx) error {
		cur, err := getContact(ctx, tx, c.tenant, u, false, true)
		if err != nil {
			return err
		}
		if err := checkVersion(ifVersion, cur.Version); err != nil {
			return err
		}
		set(&cur.FirstName, p.FirstName)
		set(&cur.LastName, p.LastName)
		set(&cur.Email, p.Email)
		set(&cur.Phone, p.Phone)
		set(&cur.Title, p.Title)
		set(&cur.OwnerSub, p.OwnerSub)
		if cur.FirstName == "" && cur.LastName == "" && cur.Email == "" {
			return invalidRef("a contact needs a name or an email")
		}
		if cur.Fields, err = crm.MergeFields(cur.Fields, p.Fields); err != nil {
			return err
		}
		r := &contactRow{}
		if err := tx.QueryRow(ctx,
			`UPDATE crm_contacts SET first_name=$3, last_name=$4, email=$5, phone=$6, title=$7, owner_sub=$8, fields=$9, version=version+1
			  WHERE tenant_id=$1 AND id=$2 RETURNING `+contactCols,
			c.tenant, u, cur.FirstName, cur.LastName, cur.Email, cur.Phone, cur.Title, cur.OwnerSub, cur.Fields).Scan(r.dest()...); err != nil {
			return err
		}
		out = r.done()
		return nil
	})
	return out, mapNotFound(err, "contact", id)
}

func (c *client) ArchiveContact(ctx context.Context, id crm.ID, ifVersion int64) error {
	return c.archive(ctx, "crm_contacts", "contact", id, ifVersion)
}

func (c *client) ListContacts(ctx context.Context, f crm.ContactFilter, p crm.PageRequest) (crm.Page[crm.Contact], error) {
	size, after, err := pageArgs(p)
	if err != nil {
		return crm.Page[crm.Contact]{}, err
	}
	var out crm.Page[crm.Contact]
	err = c.tx(ctx, func(tx pgx.Tx) (err error) {
		q := newList("crm_contacts", contactCols, c.tenant, after, !f.IncludeArchived)
		if !q.idEq("company_id", f.CompanyID) {
			return nil // a malformed id matches nothing
		}
		q.search(f.Query, "(first_name || ' ' || last_name)", "email")
		q.textEq("owner_sub", f.OwnerSub)
		out, err = runList(ctx, tx, q, size, func() ([]any, func() crm.Contact) { r := &contactRow{}; return r.dest(), r.done })
		return err
	})
	return out, err
}

// ───────────────────────────── shared error plumbing ─────────────────────────────

// mapNotFound turns the bare ErrNotFound from fetch into one that names the record.
func mapNotFound(err error, kind string, id crm.ID) error {
	if err == crm.ErrNotFound {
		return notFound(kind, id)
	}
	return err
}

func orMissing(err error) error {
	if err != nil {
		return err
	}
	return crm.ErrNotFound // unreachable in practice: the key vanished between two statements of one transaction
}

func utc(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

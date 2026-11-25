package crmops

import (
	"context"

	"github.com/mrbiz/venture-os/internal/crm"
)

// Human-only operations. They exist for the Command Center and are NOT offered to agents: editing the identity of a
// customer record and archiving records are judgement calls a person makes. (Agents can create and annotate; see
// the package comment for the policy.) The control plane refuses them on the agent path as if they did not exist, and
// the MCP bus never lists them.

// ForAgents returns the operations an agent may use, in order.
func ForAgents() []Op {
	var out []Op
	for _, o := range All() {
		if !o.HumanOnly {
			out = append(out, o)
		}
	}
	return out
}

func humanOnly(o Op) Op { o.HumanOnly = true; return o }

func humanOps() []Op {
	return []Op{updateCompanyOp(), updateContactOp(), archiveOp("crm_archive_company", "company", func(ctx context.Context, c crm.CRM, id crm.ID, v int64) error {
		return c.ArchiveCompany(ctx, id, v)
	}), archiveOp("crm_archive_contact", "contact", func(ctx context.Context, c crm.CRM, id crm.ID, v int64) error {
		return c.ArchiveContact(ctx, id, v)
	}), archiveOp("crm_archive_deal", "deal", func(ctx context.Context, c crm.CRM, id crm.ID, v int64) error {
		return c.ArchiveDeal(ctx, id, v)
	})}
}

func updateCompanyOp() Op {
	type args struct {
		CompanyID       string            `json:"company_id"`
		Name            *string           `json:"name"`
		Domain          *string           `json:"domain"`
		Country         *string           `json:"country"`
		Industry        *string           `json:"industry"`
		Website         *string           `json:"website"`
		Phone           *string           `json:"phone"`
		Fields          map[string]string `json:"fields"`
		ExpectedVersion int64             `json:"expected_version"`
	}
	return humanOnly(op("crm_update_company", false,
		"Change a company's details. Only what is passed changes; an empty string clears an optional detail.",
		map[string]any{
			"company_id": str("The company id."), "name": str("Name."), "domain": str("Web domain."), "country": str("ISO 3166-1 alpha-2 code."),
			"industry": str("Industry."), "website": str("Website URL."), "phone": str("Phone."),
			"fields":           fieldsProp("Extra facts; an empty value removes a key."),
			"expected_version": integer("The version this change is based on; the update fails if the record changed since.", 1),
		}, []string{"company_id"},
		func(ctx context.Context, c crm.CRM, _ string, a args) (Result, error) {
			co, err := c.UpdateCompany(ctx, crm.ID(a.CompanyID), crm.CompanyPatch{Name: a.Name, Domain: a.Domain, Country: a.Country,
				Industry: a.Industry, Website: a.Website, Phone: a.Phone, Fields: a.Fields}, a.ExpectedVersion)
			if err != nil {
				return Result{}, err
			}
			return Result{Data: map[string]any{"company": viewCompany(co)}, EntityID: string(co.ID)}, nil
		}))
}

func updateContactOp() Op {
	type args struct {
		ContactID       string            `json:"contact_id"`
		FirstName       *string           `json:"first_name"`
		LastName        *string           `json:"last_name"`
		Email           *string           `json:"email"`
		Phone           *string           `json:"phone"`
		Title           *string           `json:"title"`
		Fields          map[string]string `json:"fields"`
		ExpectedVersion int64             `json:"expected_version"`
	}
	return humanOnly(op("crm_update_contact", false,
		"Change a contact's details. Only what is passed changes. A contact keeps its company, and must keep a name or an email.",
		map[string]any{
			"contact_id": str("The contact id."), "first_name": str("First name."), "last_name": str("Last name."), "email": str("Email address."),
			"phone": str("Phone."), "title": str("Job title."), "fields": fieldsProp("Extra facts; an empty value removes a key."),
			"expected_version": integer("The version this change is based on; the update fails if the record changed since.", 1),
		}, []string{"contact_id"},
		func(ctx context.Context, c crm.CRM, _ string, a args) (Result, error) {
			ct, err := c.UpdateContact(ctx, crm.ID(a.ContactID), crm.ContactPatch{FirstName: a.FirstName, LastName: a.LastName,
				Email: a.Email, Phone: a.Phone, Title: a.Title, Fields: a.Fields}, a.ExpectedVersion)
			if err != nil {
				return Result{}, err
			}
			return Result{Data: map[string]any{"contact": viewContact(ct)}, EntityID: string(ct.ID)}, nil
		}))
}

// archiveOp builds crm_archive_<kind>. Archiving hides a record from lists and reads but keeps its history; it does
// not archive what hangs off it (a company's deals stay in the pipeline), so callers should say so to the user.
func archiveOp(name, kind string, do func(ctx context.Context, c crm.CRM, id crm.ID, version int64) error) Op {
	type args struct {
		ID              string `json:"id"`
		ExpectedVersion int64  `json:"expected_version"`
	}
	return humanOnly(op(name, false,
		"Archive a "+kind+": it disappears from lists and searches but its history is kept. This does not archive records attached to it.",
		map[string]any{"id": str("The " + kind + " id."), "expected_version": integer("The version this change is based on; fails if the record changed since.", 1)},
		[]string{"id"},
		func(ctx context.Context, c crm.CRM, _ string, a args) (Result, error) {
			if err := do(ctx, c, crm.ID(a.ID), a.ExpectedVersion); err != nil {
				return Result{}, err
			}
			return Result{Data: map[string]any{"archived": true, "id": a.ID}, EntityID: a.ID}, nil
		}))
}

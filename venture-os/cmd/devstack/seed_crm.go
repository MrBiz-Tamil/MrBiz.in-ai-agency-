package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/crm"
)

// seedCRM gives the demo export business a believable sales pipeline, through the same crm.CRM interface the
// Command Center and agents use (so the seed also exercises it). Idempotent: every create carries a fixed key.
func seedCRM(ctx context.Context, p crm.Provider, tenant uuid.UUID) error {
	c, err := p.For(ctx, tenant)
	if err != nil {
		return err
	}
	company := func(key, name, domain, country string) (crm.Company, error) {
		co, _, err := c.CreateCompany(ctx, "seed-co-"+key, crm.CompanyInput{Name: name, Domain: domain, Country: country, Industry: "Spice importer"})
		return co, err
	}
	type spec struct {
		key, name, company, stage, incoterm, port string
		amount                                    int64
		currency                                  string
		closeIn                                   int
	}
	cos := map[string]crm.Company{}
	for _, x := range [][4]string{
		{"berlin", "Berlin Gewürze GmbH", "berlin-gewuerze.de", "DE"},
		{"hamburg", "Hamburg Importers AG", "hamburg-importers.de", "DE"},
		{"nordic", "Nordic Spice Importers AB", "nordicspice.se", "SE"},
		{"rotterdam", "Rotterdam Fine Foods BV", "rotterdamfine.nl", "NL"},
		{"dubai", "Al Noor Trading LLC", "alnoor-trading.ae", "AE"},
		{"chicago", "Lakeshore Provisions Inc", "lakeshoreprov.com", "US"},
	} {
		co, err := company(x[0], x[1], x[2], x[3])
		if err != nil {
			return fmt.Errorf("company %s: %w", x[0], err)
		}
		cos[x[0]] = co
	}
	for key, k := range map[string]crm.ContactInput{
		"berlin":  {CompanyID: cos["berlin"].ID, FirstName: "Katrin", LastName: "Vogel", Email: "k.vogel@berlin-gewuerze.de", Title: "Head of Purchasing"},
		"nordic":  {CompanyID: cos["nordic"].ID, FirstName: "Astrid", LastName: "Lindqvist", Email: "astrid@nordicspice.se", Title: "Buyer"},
		"hamburg": {CompanyID: cos["hamburg"].ID, FirstName: "Jonas", LastName: "Brandt", Email: "jonas.brandt@hamburg-importers.de"},
	} {
		if _, _, err := c.CreateContact(ctx, "seed-ct-"+key, k); err != nil {
			return fmt.Errorf("contact %s: %w", key, err)
		}
	}

	now := time.Now().UTC()
	deals := []spec{
		{"d1", "Black pepper, 2 x 20ft", "berlin", "po_received", "FOB", "Hamburg", 4800000, "USD", 10},
		{"d2", "Cardamom 8mm, 500 kg", "hamburg", "quoted", "CIF", "Hamburg", 1850000, "USD", 21},
		{"d3", "Turmeric powder, 5 tonnes", "nordic", "sample_sent", "CIF", "Gothenburg", 2250000, "EUR", 30},
		{"d4", "Mixed whole spices trial", "rotterdam", "inquiry", "FOB", "Rotterdam", 0, "", 45},
		{"d5", "Cloves, 1 tonne", "dubai", "quoted", "CIF", "Jebel Ali", 960000, "USD", 14},
		{"d6", "Black pepper, 1 x 20ft", "chicago", "inquiry", "FOB", "Houston", 2400000, "USD", 60},
		{"d7", "Nutmeg & mace sampler", "nordic", "shipped", "DDP", "Gothenburg", 380000, "EUR", -5},
	}
	ids := map[string]crm.Deal{}
	for _, d := range deals {
		in := crm.DealInput{
			Name: d.name, CompanyID: cos[d.company].ID, Pipeline: "export_deals", Stage: "inquiry",
			CreatedBy: "agent:sales", ExpectedClose: now.AddDate(0, 0, d.closeIn), Fields: map[string]string{"incoterm": d.incoterm, "destination_port": d.port},
		}
		if d.amount > 0 {
			in.Amount = &crm.Money{Minor: d.amount, Currency: d.currency}
		}
		deal, _, err := c.CreateDeal(ctx, "seed-deal-"+d.key, in)
		if err != nil {
			return fmt.Errorf("deal %s: %w", d.key, err)
		}
		// Walk it through the stages like a real deal would, so the history is real too.
		for _, stage := range []string{"quoted", "sample_sent", "po_received", "shipped"} {
			if deal.Stage == d.stage {
				break
			}
			if deal, err = c.MoveDeal(ctx, deal.ID, stage, "agent:sales", 0); err != nil {
				return fmt.Errorf("move %s: %w", d.key, err)
			}
		}
		ids[d.key] = deal
	}

	act := func(key string, in crm.ActivityInput) error {
		_, _, err := c.CreateActivity(ctx, "seed-act-"+key, in)
		return err
	}
	for _, a := range []struct {
		key string
		in  crm.ActivityInput
	}{
		{"a1", crm.ActivityInput{Kind: crm.ActivityCall, Subject: "Intro call with Katrin", Body: "Wants 2 x 20ft of Tellicherry. Asked for CIF Hamburg and a sample before the PO.", DealID: ids["d1"].ID, CreatedBy: "user:sub-dev-founder"}},
		{"a2", crm.ActivityInput{Kind: crm.ActivityNote, Subject: "PO received", Body: "PO 4471 received by email. Payment: 30 days after bill of lading.", DealID: ids["d1"].ID, CreatedBy: "agent:sales"}},
		{"a3", crm.ActivityInput{Kind: crm.ActivityTask, Subject: "Book container with Maersk", DealID: ids["d1"].ID, DueAt: now.AddDate(0, 0, -2), CreatedBy: "agent:sales"}},
		{"a4", crm.ActivityInput{Kind: crm.ActivityTask, Subject: "Send revised cardamom quote", Body: "Buyer asked for a price per kg including packing.", DealID: ids["d2"].ID, DueAt: now.AddDate(0, 0, 3), CreatedBy: "user:sub-dev-founder"}},
		{"a5", crm.ActivityInput{Kind: crm.ActivityEmail, Subject: "Sample dispatch notice", Body: "Tracking DHL 4471 9920 3381 sent to Astrid.", DealID: ids["d3"].ID, CreatedBy: "agent:sales"}},
		{"a6", crm.ActivityInput{Kind: crm.ActivityTask, Subject: "Chase Lakeshore for volumes", CompanyID: cos["chicago"].ID, DueAt: now.AddDate(0, 0, 7), CreatedBy: "agent:sales"}},
	} {
		if err := act(a.key, a.in); err != nil {
			return fmt.Errorf("activity %s: %w", a.key, err)
		}
	}
	return nil
}

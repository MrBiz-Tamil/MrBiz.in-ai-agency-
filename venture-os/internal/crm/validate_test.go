package crm_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mrbiz/venture-os/internal/crm"
	"github.com/mrbiz/venture-os/internal/domain"
)

func pipelines() []crm.Pipeline {
	return []crm.Pipeline{{
		Key: "sales", Name: "Sales", EntityType: domain.EntityDeal,
		Stages: []domain.Stage{{Key: "lead", Name: "Lead"}, {Key: "won", Name: "Won"}},
		CustomFields: []domain.CustomField{
			{Key: "incoterm", Label: "Incoterm", Type: "SELECT", Required: true},
			{Key: "volume_kg", Label: "Volume", Type: "NUMBER"},
			{Key: "start_on", Label: "Start", Type: "DATE"},
			{Key: "sample_sent", Label: "Sample", Type: "BOOLEAN"},
		},
	}}
}

func TestNormalizeCompanyCanonicalizes(t *testing.T) {
	got, err := crm.NormalizeCompany(crm.CompanyInput{Name: "  Acme GmbH ", Domain: " ACME.com ", Country: "de", Fields: map[string]string{"tier": "gold"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Acme GmbH" || got.Domain != "acme.com" || got.Country != "DE" {
		t.Fatalf("got %+v", got)
	}
}

func TestInvalidInputsAreRejectedWithErrInvalid(t *testing.T) {
	long := strings.Repeat("x", crm.MaxNameLen+1)
	manyFields := map[string]string{}
	for i := 0; i < crm.MaxFields+1; i++ {
		manyFields["k"+strings.Repeat("a", i%5)+string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
	}
	cases := map[string]func() error{
		"company name empty": func() error { _, e := crm.NormalizeCompany(crm.CompanyInput{Name: "  "}); return e },
		"company name long":  func() error { _, e := crm.NormalizeCompany(crm.CompanyInput{Name: long}); return e },
		"bad domain": func() error {
			_, e := crm.NormalizeCompany(crm.CompanyInput{Name: "a", Domain: "not a domain"})
			return e
		},
		"bad country": func() error { _, e := crm.NormalizeCompany(crm.CompanyInput{Name: "a", Country: "DEU"}); return e },
		"NUL in text": func() error { _, e := crm.NormalizeCompany(crm.CompanyInput{Name: "a\x00b"}); return e },
		"bad field key": func() error {
			_, e := crm.NormalizeCompany(crm.CompanyInput{Name: "a", Fields: map[string]string{"Bad Key": "v"}})
			return e
		},
		"too many fields":   func() error { _, e := crm.NormalizeCompany(crm.CompanyInput{Name: "a", Fields: manyFields}); return e },
		"contact empty":     func() error { _, e := crm.NormalizeContact(crm.ContactInput{}); return e },
		"contact bad email": func() error { _, e := crm.NormalizeContact(crm.ContactInput{Email: "nope"}); return e },
		"patch empty name":  func() error { e := ""; _, err := crm.NormalizeCompanyPatch(crm.CompanyPatch{Name: &e}); return err },
		"activity bad kind": func() error {
			_, e := crm.NormalizeActivity(crm.ActivityInput{Kind: "SMOKE", Subject: "s", CompanyID: "c"})
			return e
		},
		"activity no parent": func() error {
			_, e := crm.NormalizeActivity(crm.ActivityInput{Kind: crm.ActivityNote, Subject: "s"})
			return e
		},
		"note with due date": func() error {
			_, e := crm.NormalizeActivity(crm.ActivityInput{Kind: crm.ActivityNote, Subject: "s", CompanyID: "c", DueAt: time.Now()})
			return e
		},
		"activity no subject": func() error {
			_, e := crm.NormalizeActivity(crm.ActivityInput{Kind: crm.ActivityCall, CompanyID: "c"})
			return e
		},
		"page too big": func() error { _, e := crm.PageSize(crm.PageRequest{Size: crm.MaxPageSize + 1}); return e },
		"key empty":    func() error { return crm.ValidateKey("  ") },
	}
	for name, f := range cases {
		if err := f(); !errors.Is(err, crm.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

func TestNormalizeDealChecksPipelineStageAndFields(t *testing.T) {
	ok := crm.DealInput{Name: "Pepper", CompanyID: "c1", Pipeline: "sales", Stage: "lead", Fields: map[string]string{"incoterm": "FOB", "volume_kg": "1500.5"}}
	if _, _, err := crm.NormalizeDeal(ok, pipelines()); err != nil {
		t.Fatalf("valid deal rejected: %v", err)
	}
	bad := map[string]func(d *crm.DealInput){
		"no company":       func(d *crm.DealInput) { d.CompanyID = "" },
		"unknown pipeline": func(d *crm.DealInput) { d.Pipeline = "nope" },
		"unknown stage":    func(d *crm.DealInput) { d.Stage = "shipped" },
		"missing required": func(d *crm.DealInput) { d.Fields = map[string]string{} },
		"undefined field":  func(d *crm.DealInput) { d.Fields["hack"] = "1" },
		"nan number":       func(d *crm.DealInput) { d.Fields["volume_kg"] = "lots" },
		"bad date":         func(d *crm.DealInput) { d.Fields["start_on"] = "01/02/2026" },
		"bad bool":         func(d *crm.DealInput) { d.Fields["sample_sent"] = "yes" },
		"negative amount":  func(d *crm.DealInput) { d.Amount = &crm.Money{Minor: -1, Currency: "USD"} },
		"bad currency":     func(d *crm.DealInput) { d.Amount = &crm.Money{Minor: 1, Currency: "dollars"} },
		"huge amount":      func(d *crm.DealInput) { d.Amount = &crm.Money{Minor: crm.MaxAmountMinor + 1, Currency: "USD"} },
	}
	for name, mutate := range bad {
		d := ok
		d.Fields = map[string]string{"incoterm": "FOB", "volume_kg": "1"}
		mutate(&d)
		if _, _, err := crm.NormalizeDeal(d, pipelines()); !errors.Is(err, crm.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

func TestDealPatchAndFieldMerging(t *testing.T) {
	p := pipelines()[0]
	empty := ""
	if _, err := crm.NormalizeDealPatch(crm.DealPatch{Fields: map[string]string{"incoterm": empty}}, p); !errors.Is(err, crm.ErrInvalid) {
		t.Errorf("removing a required field must fail: %v", err)
	}
	if _, err := crm.NormalizeDealPatch(crm.DealPatch{Fields: map[string]string{"volume_kg": empty}}, p); err != nil {
		t.Errorf("removing an optional field must work: %v", err)
	}
	if _, err := crm.NormalizeDealPatch(crm.DealPatch{Amount: &crm.Money{Minor: 1, Currency: "USD"}, ClearAmount: true}, p); !errors.Is(err, crm.ErrInvalid) {
		t.Errorf("set+clear amount: %v", err)
	}
	merged, err := crm.MergeFields(map[string]string{"a": "1", "b": "2"}, map[string]string{"b": "", "c": "3"})
	if err != nil || len(merged) != 2 || merged["a"] != "1" || merged["c"] != "3" {
		t.Errorf("merge = %v %v", merged, err)
	}
}

func TestHashInputIsStableAndSensitive(t *testing.T) {
	a := crm.CompanyInput{Name: "A", Fields: map[string]string{"x": "1", "y": "2"}}
	b := crm.CompanyInput{Name: "A", Fields: map[string]string{"y": "2", "x": "1"}}
	if crm.HashInput(a) != crm.HashInput(b) {
		t.Error("map order changed the hash")
	}
	b.Name = "B"
	if crm.HashInput(a) == crm.HashInput(b) {
		t.Error("different input, same hash")
	}
}

func TestDateTruncatesToUTCDay(t *testing.T) {
	in := time.Date(2026, 3, 1, 23, 30, 0, 0, time.FixedZone("x", 5*3600+1800))
	if got := crm.Date(in); !got.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) && !got.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date = %v", got)
	}
	if !crm.Date(time.Time{}).IsZero() {
		t.Error("zero must stay zero")
	}
}

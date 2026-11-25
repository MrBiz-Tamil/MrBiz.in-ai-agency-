package crm

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Every backend (built-in, in-memory, and external adapters) normalizes and validates input with these functions
// before touching its store, so the same input is accepted or rejected identically everywhere. That is what lets
// the contract suite treat validation as part of the interface.

var (
	domainRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
	emailRe    = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	countryRe  = regexp.MustCompile(`^[A-Z]{2}$`)
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
	fieldKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// HashInput is the identity of a request, stored with its idempotency key to detect key reuse.
func HashInput(v any) [32]byte {
	raw, _ := json.Marshal(v) // maps marshal with sorted keys, structs in field order: deterministic
	return sha256.Sum256(raw)
}

// ValidateKey checks an idempotency key.
func ValidateKey(key string) error {
	if strings.TrimSpace(key) == "" || len(key) > MaxIdempotencyKey {
		return invalid("idempotency key is required (max %d chars)", MaxIdempotencyKey)
	}
	return nil
}

func text(field, v string, max int, required bool) (string, error) {
	v = strings.TrimSpace(v)
	if required && v == "" {
		return "", invalid("%s is required", field)
	}
	if utf8.RuneCountInString(v) > max { // characters, not bytes: Devanagari and emoji are 3-4 bytes each
		return "", invalid("%s is longer than %d characters", field, max)
	}
	if strings.ContainsRune(v, 0) {
		return "", invalid("%s contains a NUL character", field)
	}
	return v, nil
}

// ValidateFields checks the shape of a custom-field map (keys, sizes, count). Pipeline-specific rules for deals
// are applied separately by NormalizeDeal.
func ValidateFields(f map[string]string) error {
	if len(f) > MaxFields {
		return invalid("at most %d custom fields", MaxFields)
	}
	for k, v := range f {
		if !fieldKeyRe.MatchString(k) {
			return invalid("custom field key %q must match %s", k, fieldKeyRe)
		}
		if utf8.RuneCountInString(v) > MaxTextLen || strings.ContainsRune(v, 0) {
			return invalid("custom field %q is too long or contains NUL", k)
		}
	}
	return nil
}

// MergeFields applies a fields patch to the current fields: entries are merged and an empty value removes the key.
func MergeFields(current, patch map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(current)+len(patch))
	for k, v := range current {
		out[k] = v
	}
	for k, v := range patch {
		if v == "" {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	if err := ValidateFields(out); err != nil {
		return nil, err
	}
	return out, nil
}

func cloneFields(f map[string]string) map[string]string {
	out := make(map[string]string, len(f))
	for k, v := range f {
		out[k] = v
	}
	return out
}

// ───────────────────────────── companies ─────────────────────────────

func normalizeDomain(d string) (string, error) {
	d = strings.ToLower(strings.TrimSpace(d))
	if d != "" && !domainRe.MatchString(d) {
		return "", invalid("domain %q is not a valid hostname", d)
	}
	return d, nil
}

func normalizeCountry(c string) (string, error) {
	c = strings.ToUpper(strings.TrimSpace(c))
	if c != "" && !countryRe.MatchString(c) {
		return "", invalid("country %q must be an ISO 3166-1 alpha-2 code", c)
	}
	return c, nil
}

// NormalizeCompany validates and canonicalizes a company input.
func NormalizeCompany(in CompanyInput) (CompanyInput, error) {
	var err error
	if in.Name, err = text("name", in.Name, MaxNameLen, true); err != nil {
		return in, err
	}
	if in.Domain, err = normalizeDomain(in.Domain); err != nil {
		return in, err
	}
	if in.Country, err = normalizeCountry(in.Country); err != nil {
		return in, err
	}
	for _, f := range []struct {
		name string
		v    *string
		max  int
	}{{"industry", &in.Industry, 100}, {"website", &in.Website, 500}, {"phone", &in.Phone, 50}, {"owner", &in.OwnerSub, 200}} {
		if *f.v, err = text(f.name, *f.v, f.max, false); err != nil {
			return in, err
		}
	}
	if err := ValidateFields(in.Fields); err != nil {
		return in, err
	}
	in.Fields = cloneFields(in.Fields)
	return in, nil
}

// NormalizeCompanyPatch validates a patch (only the provided fields) and canonicalizes it.
func NormalizeCompanyPatch(p CompanyPatch) (CompanyPatch, error) {
	var err error
	if p.Name != nil {
		v, e := text("name", *p.Name, MaxNameLen, true)
		if e != nil {
			return p, e
		}
		p.Name = &v
	}
	if p.Domain != nil {
		v, e := normalizeDomain(*p.Domain)
		if e != nil {
			return p, e
		}
		p.Domain = &v
	}
	if p.Country != nil {
		v, e := normalizeCountry(*p.Country)
		if e != nil {
			return p, e
		}
		p.Country = &v
	}
	for _, f := range []struct {
		name string
		v    **string
		max  int
	}{{"industry", &p.Industry, 100}, {"website", &p.Website, 500}, {"phone", &p.Phone, 50}, {"owner", &p.OwnerSub, 200}} {
		if *f.v != nil {
			v, e := text(f.name, **f.v, f.max, false)
			if e != nil {
				return p, e
			}
			*f.v = &v
		}
	}
	if err = ValidateFields(p.Fields); err != nil {
		return p, err
	}
	return p, nil
}

// ───────────────────────────── contacts ─────────────────────────────

func normalizeEmail(e string) (string, error) {
	e = strings.ToLower(strings.TrimSpace(e))
	if e != "" && (len(e) > 254 || !emailRe.MatchString(e)) {
		return "", invalid("email %q is not valid", e)
	}
	return e, nil
}

// NormalizeContact validates and canonicalizes a contact input.
func NormalizeContact(in ContactInput) (ContactInput, error) {
	var err error
	for _, f := range []struct {
		name string
		v    *string
		max  int
	}{{"first name", &in.FirstName, 100}, {"last name", &in.LastName, 100}, {"phone", &in.Phone, 50}, {"title", &in.Title, 100}, {"owner", &in.OwnerSub, 200}} {
		if *f.v, err = text(f.name, *f.v, f.max, false); err != nil {
			return in, err
		}
	}
	if in.Email, err = normalizeEmail(in.Email); err != nil {
		return in, err
	}
	if in.FirstName == "" && in.LastName == "" && in.Email == "" {
		return in, invalid("a contact needs a name or an email")
	}
	if err := ValidateFields(in.Fields); err != nil {
		return in, err
	}
	in.Fields = cloneFields(in.Fields)
	return in, nil
}

// NormalizeContactPatch validates and canonicalizes a contact patch.
func NormalizeContactPatch(p ContactPatch) (ContactPatch, error) {
	for _, f := range []struct {
		name string
		v    **string
		max  int
	}{{"first name", &p.FirstName, 100}, {"last name", &p.LastName, 100}, {"phone", &p.Phone, 50}, {"title", &p.Title, 100}, {"owner", &p.OwnerSub, 200}} {
		if *f.v != nil {
			v, e := text(f.name, **f.v, f.max, false)
			if e != nil {
				return p, e
			}
			*f.v = &v
		}
	}
	if p.Email != nil {
		v, e := normalizeEmail(*p.Email)
		if e != nil {
			return p, e
		}
		p.Email = &v
	}
	return p, ValidateFields(p.Fields)
}

// ───────────────────────────── deals ─────────────────────────────

// FindPipeline returns the pipeline with the given key.
func FindPipeline(ps []Pipeline, key string) (Pipeline, bool) {
	for _, p := range ps {
		if p.Key == key {
			return p, true
		}
	}
	return Pipeline{}, false
}

// HasStage reports whether the pipeline defines the stage.
func HasStage(p Pipeline, stage string) bool {
	for _, s := range p.Stages {
		if s.Key == stage {
			return true
		}
	}
	return false
}

func normalizeMoney(m *Money) (*Money, error) {
	if m == nil {
		return nil, nil
	}
	if m.Minor < 0 || m.Minor > MaxAmountMinor {
		return nil, invalid("amount must be between 0 and %d minor units", MaxAmountMinor)
	}
	cur := strings.ToUpper(strings.TrimSpace(m.Currency))
	if !currencyRe.MatchString(cur) {
		return nil, invalid("currency %q must be an ISO 4217 code", m.Currency)
	}
	return &Money{Minor: m.Minor, Currency: cur}, nil
}

// Date truncates a time to its UTC calendar date; the zero time stays zero.
func Date(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func checkFieldValue(typ, key, v string) error {
	switch typ {
	case "NUMBER":
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return invalid("custom field %q must be a number, got %q", key, v)
		}
	case "DATE":
		if _, err := time.Parse("2006-01-02", v); err != nil {
			return invalid("custom field %q must be a date (YYYY-MM-DD), got %q", key, v)
		}
	case "BOOLEAN":
		if v != "true" && v != "false" {
			return invalid("custom field %q must be true or false, got %q", key, v)
		}
	}
	return nil
}

// validateDealFields checks fields against the pipeline's definition. With requireAll, every required field must
// be present and non-empty (creation); without it only the supplied keys are checked (patches, where an empty value
// is allowed only for optional fields because it means "remove").
func validateDealFields(p Pipeline, fields map[string]string, requireAll bool) error {
	defs := map[string]struct {
		typ      string
		required bool
	}{}
	for _, f := range p.CustomFields {
		defs[f.Key] = struct {
			typ      string
			required bool
		}{f.Type, f.Required}
	}
	for k, v := range fields {
		d, ok := defs[k]
		if !ok {
			return invalid("pipeline %q has no custom field %q", p.Key, k)
		}
		if v == "" {
			if d.required {
				return invalid("custom field %q is required", k)
			}
			continue
		}
		if err := checkFieldValue(d.typ, k, v); err != nil {
			return err
		}
	}
	if requireAll {
		for _, f := range p.CustomFields {
			if f.Required && fields[f.Key] == "" {
				return invalid("custom field %q is required by pipeline %q", f.Key, p.Key)
			}
		}
	}
	return nil
}

// NormalizeDeal validates a deal input against the tenant's pipelines.
func NormalizeDeal(in DealInput, pipelines []Pipeline) (DealInput, Pipeline, error) {
	var err error
	if in.Name, err = text("name", in.Name, MaxNameLen, true); err != nil {
		return in, Pipeline{}, err
	}
	if in.OwnerSub, err = text("owner", in.OwnerSub, 200, false); err != nil {
		return in, Pipeline{}, err
	}
	if in.CreatedBy, err = text("created by", in.CreatedBy, 200, false); err != nil {
		return in, Pipeline{}, err
	}
	if strings.TrimSpace(string(in.CompanyID)) == "" {
		return in, Pipeline{}, invalid("a deal needs a company")
	}
	p, ok := FindPipeline(pipelines, in.Pipeline)
	if !ok {
		return in, Pipeline{}, invalid("unknown pipeline %q", in.Pipeline)
	}
	if !HasStage(p, in.Stage) {
		return in, Pipeline{}, invalid("pipeline %q has no stage %q", in.Pipeline, in.Stage)
	}
	if in.Amount, err = normalizeMoney(in.Amount); err != nil {
		return in, Pipeline{}, err
	}
	in.ExpectedClose = Date(in.ExpectedClose)
	if err := ValidateFields(in.Fields); err != nil {
		return in, Pipeline{}, err
	}
	if err := validateDealFields(p, in.Fields, true); err != nil {
		return in, Pipeline{}, err
	}
	in.Fields = cloneFields(in.Fields)
	return in, p, nil
}

// NormalizeDealPatch validates a deal patch; the pipeline is the existing deal's.
func NormalizeDealPatch(pt DealPatch, p Pipeline) (DealPatch, error) {
	if pt.Name != nil {
		v, e := text("name", *pt.Name, MaxNameLen, true)
		if e != nil {
			return pt, e
		}
		pt.Name = &v
	}
	if pt.OwnerSub != nil {
		v, e := text("owner", *pt.OwnerSub, 200, false)
		if e != nil {
			return pt, e
		}
		pt.OwnerSub = &v
	}
	if pt.Amount != nil && pt.ClearAmount {
		return pt, invalid("set the amount or clear it, not both")
	}
	if pt.ExpectedClose != nil && pt.ClearExpectedClose {
		return pt, invalid("set the expected close date or clear it, not both")
	}
	var err error
	if pt.Amount, err = normalizeMoney(pt.Amount); err != nil {
		return pt, err
	}
	if pt.ExpectedClose != nil {
		d := Date(*pt.ExpectedClose)
		pt.ExpectedClose = &d
	}
	if err := ValidateFields(pt.Fields); err != nil {
		return pt, err
	}
	return pt, validateDealFields(p, pt.Fields, false)
}

// ───────────────────────────── activities ─────────────────────────────

// NormalizeActivity validates an activity input.
func NormalizeActivity(in ActivityInput) (ActivityInput, error) {
	switch in.Kind {
	case ActivityNote, ActivityCall, ActivityEmail, ActivityMeeting, ActivityTask:
	default:
		return in, invalid("unknown activity kind %q", in.Kind)
	}
	var err error
	if in.Subject, err = text("subject", in.Subject, MaxNameLen, true); err != nil {
		return in, err
	}
	if in.Body, err = text("body", in.Body, MaxBodyLen, false); err != nil {
		return in, err
	}
	if in.OwnerSub, err = text("owner", in.OwnerSub, 200, false); err != nil {
		return in, err
	}
	if in.CreatedBy, err = text("created by", in.CreatedBy, 200, false); err != nil {
		return in, err
	}
	if !in.DueAt.IsZero() && in.Kind != ActivityTask {
		return in, invalid("only a TASK can have a due date")
	}
	if in.CompanyID == "" && in.ContactID == "" && in.DealID == "" {
		return in, invalid("an activity must belong to a company, contact or deal")
	}
	in.DueAt = in.DueAt.UTC()
	return in, nil
}

// ───────────────────────────── paging ─────────────────────────────

const seqCursorPrefix = "c1:"

// SeqCursor encodes a paging position that is a monotonically increasing sequence number (the position of the last
// item on the page). Backends whose paging is keyed on such a number share it; the result is opaque to callers.
func SeqCursor(seq int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(seqCursorPrefix + strconv.FormatInt(seq, 10)))
}

// ParseSeqCursor decodes SeqCursor output. The empty cursor means "from the start" (0). Anything else that does
// not decode is ErrInvalid.
func ParseSeqCursor(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || !strings.HasPrefix(string(raw), seqCursorPrefix) {
		return 0, invalid("malformed page cursor")
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(string(raw), seqCursorPrefix), 10, 64)
	if err != nil || n < 0 {
		return 0, invalid("malformed page cursor")
	}
	return n, nil
}

// PageSize validates a page request and returns the effective size.
func PageSize(p PageRequest) (int, error) {
	switch {
	case p.Size < 0 || p.Size > MaxPageSize:
		return 0, invalid("page size must be between 1 and %d", MaxPageSize)
	case p.Size == 0:
		return DefaultPageSize, nil
	}
	return p.Size, nil
}

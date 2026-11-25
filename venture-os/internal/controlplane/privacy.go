package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/dlp"
)

// screenedFields are the keys whose string values are free text typed by people outside the business (customers, in
// the notes and tasks staff or agents log about them): a CRM activity's subject and body. They are the text most likely
// to carry a prompt injection or a pasted secret, and nothing an agent needs to act on a customer lives in them.
// Structured fields (a contact's e-mail and phone, a company's name, an amount) are NOT screened: an agent that handles
// a customer needs them, and redacting them would break the product (docs/DLP.md, "Where to connect it").
var screenedFields = map[string]bool{"subject": true, "body": true}

// screenStats says what the filter did, never what it saw.
type screenStats struct {
	fields    int            // free-text values screened
	redacted  map[string]int // by kind: how many items of personal data or credentials were replaced
	withheld  int            // values withheld whole because they looked like a prompt injection
	oversized int            // values too large to scan, withheld whole
}

// screenForAgent passes the free text in an operation's JSON result through the privacy filter before an AGENT sees it:
// personal data and credentials are replaced by [REDACTED_<TYPE>], text that looks like a prompt injection is withheld
// entirely, and what is left is quarantined in an <untrusted_context> block with its markup neutralised. People using the
// Command Center read the original text: this is applied on the agent path only.
//
// It does nothing when s.Privacy is nil. When it is on it fails CLOSED: a result that cannot be screened is an error, never
// passed on raw.
func (s *Server) screenForAgent(ctx context.Context, tenant uuid.UUID, origin string, result []byte) ([]byte, error) {
	if s.Privacy == nil || len(result) == 0 {
		return result, nil
	}
	dec := json.NewDecoder(bytes.NewReader(result))
	dec.UseNumber() // keep every number exactly as it was (money is integer minor units)
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("privacy: result is not JSON: %w", err)
	}
	st := &screenStats{redacted: map[string]int{}}
	v = s.screenValue(v, origin, st)
	if st.fields == 0 {
		return result, nil
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("privacy: re-encode: %w", err)
	}
	if st.withheld > 0 || st.oversized > 0 || len(st.redacted) > 0 {
		s.log().Info("agent text screened", "tenant", tenant, "origin", origin, "fields", st.fields,
			"redacted", st.redacted, "withheld", st.withheld, "too_large", st.oversized)
	}
	if st.withheld > 0 {
		// A withheld injection is a security event the founder should be able to see. Counts only, never the text.
		if err := s.Repo.AuditActor(ctx, tenant, "SYSTEM", "privacy", "privacy.injection_withheld", origin, "",
			map[string]any{"withheld": st.withheld, "fields_screened": st.fields}); err != nil {
			s.log().Error("audit write failed", "action", "privacy.injection_withheld", "err", err)
		}
	}
	return out, nil
}

func (s *Server) screenValue(v any, origin string, st *screenStats) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if str, ok := x.(string); ok && screenedFields[k] && str != "" {
				t[k] = s.screenText(str, origin, st)
				continue
			}
			t[k] = s.screenValue(x, origin, st)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = s.screenValue(x, origin, st)
		}
		return t
	}
	return v
}

func (s *Server) screenText(text, origin string, st *screenStats) string {
	st.fields++
	res, err := s.Privacy.Inspect(text, origin)
	if err != nil { // dlp.ErrTooLarge: a partial scan would let the unscanned part through, so withhold the lot
		st.oversized++
		return dlp.Withheld
	}
	for kind, n := range res.Counts {
		st.redacted[kind] += n
	}
	if res.Injection {
		st.withheld++
	}
	return res.Text
}

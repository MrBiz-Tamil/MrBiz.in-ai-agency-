// Package dlp is the privacy filter for text that comes from outside the business (a customer's email, a webhook body, a
// CRM note) before an agent or a log sees it.
//
// It does three things, in this order:
//
//  1. Redacts personal and secret data: payment cards (Luhn-checked), Aadhaar numbers, PAN, e-mail addresses, Indian
//     mobile numbers, and credentials (API keys, tokens, private keys). Detection runs AFTER Unicode normalisation, so
//     full-width letters, Devanagari or Arabic-Indic digits, zero-width characters and look-alike letters do not hide data.
//  2. Screens for prompt-injection phrases. Hostile text is withheld entirely, not passed on in a wrapper.
//  3. Quarantines what is left in an <untrusted_context origin="..."> block with markup neutralised, so the text cannot
//     close the block or open another.
//
// THIS IS A HEURISTIC FIRST LINE OF DEFENCE. It lowers risk; it cannot prove that text is harmless. A determined attacker can
// phrase an instruction no pattern knows, and a number the filter does not recognise is not redacted. Everything that matters
// must still be enforced elsewhere: agents have no tool that moves money without a human approval, and tools take no tenant
// argument. Nothing in this package is a substitute for that.
//
// Descended from ai-service/security/dlp_shield.py in the earlier ABOS prototype, with the same output format. Differences
// are listed in docs/DLP.md.
package dlp

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Finding types. They appear in audit records, so they are stable.
const (
	TypeCreditCard = "CREDIT_CARD"
	TypeAadhaar    = "AADHAAR"
	TypePAN        = "PAN_IN"
	TypeEmail      = "EMAIL"
	TypePhone      = "PHONE_IN"
	TypeSecret     = "SECRET"
)

// typeOrder fixes the order findings are reported in.
var typeOrder = []string{TypeCreditCard, TypeAadhaar, TypePAN, TypeEmail, TypePhone, TypeSecret}

// MaxInputRunes bounds what Inspect will look at. Larger input is refused, not truncated: a partial scan would let
// the unscanned tail through.
const MaxInputRunes = 100_000

// ErrTooLarge is returned for input over MaxInputRunes.
var ErrTooLarge = errors.New("dlp: content is too large to inspect")

// Withheld is the body used in place of text that looks like a prompt injection.
const Withheld = "[CONTENT WITHHELD: possible prompt injection]"

// Result is the outcome of inspecting one piece of text.
type Result struct {
	// Safe is false when an injection pattern matched.
	Safe bool
	// Injection reports whether an injection pattern matched. It is the inverse of Safe, kept for readability at call sites.
	Injection bool
	// Text is what may be placed in a prompt: the redacted content (or Withheld), HTML-escaped, inside the quarantine tag.
	Text string
	// PII lists the finding types that were redacted, in a fixed order. It never contains the data itself.
	PII []string
	// Counts is how many of each type were redacted.
	Counts map[string]int
}

// Shield holds the compiled patterns. It is safe for concurrent use.
type Shield struct{}

// New returns a Shield.
func New() *Shield { return &Shield{} }

// ───────────────────────────── patterns ─────────────────────────────

var (
	emailRE = regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)
	// Any five capitals, four digits, one capital. The holder-type letter is deliberately NOT checked: test and sample PANs
	// (ABCDE1234F) break that rule and customers type them, and over-redacting is the safe error here.
	panRE = regexp.MustCompile(`\b[A-Z]{5}[0-9]{4}[A-Z]\b`)

	secretREs = []*regexp.Regexp{
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[A-Za-z0-9+/=\s]*`), // a block cut off before its END line: the header and the key material after it
		regexp.MustCompile(`\bsk-(?:ant-|proj-|live-|test-)?[A-Za-z0-9_-]{20,}`),
		regexp.MustCompile(`\b[sr]k_(?:live|test)_[A-Za-z0-9]{16,}`),
		regexp.MustCompile(`\brzp_(?:live|test)_[A-Za-z0-9]{10,}`),
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`),
		regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
		regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{20,}=*`),
		regexp.MustCompile(`(?i)\b(?:api[_-]?key|secret|passw(?:or)?d|token)\b\s*[:=]\s*\S{8,}`),
	}

	// Phrases that ask a model to drop its instructions. Matched case-insensitively against the text as given and against
	// a normalised skeleton of it (see skeleton). RE2 has no lookaround, which these do not need.
	injectionRE = compileAny(`(?is)`, []string{
		`ignore (?:all |any |the )?(?:previous|prior|above|earlier) (?:instructions|prompts?|rules)`,
		`disregard (?:all |any |the )?(?:previous|prior|above|earlier|your) (?:instructions|prompts?|rules)`,
		`forget (?:all |everything |your )?(?:previous|prior|above|earlier)? ?(?:instructions|rules)`,
		`system ?prompt`,
		`developer mode`,
		`jailbreak`,
		`you are now (?:in|a|an|the|my|dan|free|unrestricted|root|admin)\b`,
		`act as (?:an? )?(?:unrestricted|admin|root|system)`,
		`bypass (?:all )?(?:guardrails|safety|policy|policies|approval)`,
		`(?:reveal|print|show|leak) (?:your |the )?(?:secret|system|hidden|api)[ -]?(?:key|prompt|instructions)`,
		`<\s*/?\s*(?:script|iframe|untrusted_context|system|assistant)`,
		`approve (?:this |the )?(?:refund|order|ticket|payment) (?:immediately|without)`,
	})
	// A line that starts like a chat turn ("system: ..."). Checked per line, because the skeleton flattens newlines.
	roleLineRE = regexp.MustCompile(`(?i)^ ?(?:system|assistant) ?:`)

	originUnsafeRE = regexp.MustCompile(`[^A-Za-z0-9_.-]`)
)

// compileAny joins the patterns into one alternation, so a text is scanned once instead of once per pattern.
func compileAny(flags string, patterns []string) *regexp.Regexp {
	parts := make([]string, len(patterns))
	for i, p := range patterns {
		parts[i] = "(?:" + p + ")"
	}
	return regexp.MustCompile(flags + strings.Join(parts, "|"))
}

// ───────────────────────────── normalisation ─────────────────────────────

// clean returns the text in a form where detection cannot be dodged by encoding tricks, while keeping case and spacing so
// the result is still readable: NFKC (full-width and compatibility characters become ordinary ones), invisible formatting
// characters removed (zero-width spaces and joiners, soft hyphens, bidi controls), and decimal digits of every script
// turned into ASCII digits.
func clean(s string) string {
	s = strings.ToValidUTF8(s, "�")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r < utf8.RuneSelf:
			b.WriteRune(r)
		case unicode.Is(unicode.Cf, r):
			// invisible: drop
		case unicode.IsDigit(r): // category Nd, so it has a decimal value
			if d, ok := digitValue(r); ok {
				b.WriteByte(byte('0' + d))
			} else {
				b.WriteRune(r)
			}
		default:
			b.WriteRune(r)
		}
	}
	// Normalise AFTER dropping invisible characters: removing one can bring two pieces together that then compose
	// ("e", zero-width space, acute accent). Normalising only before would leave text that a second pass still changes.
	return norm.NFKC.String(b.String())
}

// digitValue returns the value 0-9 of a non-ASCII decimal digit. Unicode guarantees that decimal digits sit in runs of
// ten starting at zero, so the value is the distance from the start of its range, modulo ten.
func digitValue(r rune) (int, bool) {
	if r <= 0xFFFF {
		for _, rg := range unicode.Nd.R16 {
			if rg.Stride == 1 && uint16(r) >= rg.Lo && uint16(r) <= rg.Hi {
				return int(uint16(r)-rg.Lo) % 10, true
			}
		}
		return 0, false
	}
	for _, rg := range unicode.Nd.R32 {
		if rg.Stride == 1 && uint32(r) >= rg.Lo && uint32(r) <= rg.Hi {
			return int(uint32(r)-rg.Lo) % 10, true
		}
	}
	return 0, false
}

// confusables maps letters that look like Latin ones (Cyrillic and Greek, lower-cased) to the Latin letter. NFKC does not
// do this, and "ignоre" with a Cyrillic о would otherwise slip past every phrase. Not exhaustive.
var confusables = map[rune]rune{
	'а': 'a', 'е': 'e', 'о': 'o', 'р': 'p', 'с': 'c', 'х': 'x', 'у': 'y', 'і': 'i', 'ј': 'j', 'ѕ': 's', 'ԁ': 'd',
	'һ': 'h', 'ӏ': 'l', 'к': 'k', 'м': 'm', 'т': 't', 'ԛ': 'q', 'ԝ': 'w',
	'α': 'a', 'ε': 'e', 'ο': 'o', 'ρ': 'p', 'ι': 'i', 'κ': 'k', 'ν': 'v', 'τ': 't', 'υ': 'u', 'χ': 'x', 'β': 'b',
}

// skeleton is the form injection patterns are also tried against: no accents or combining marks, lower case, look-alike
// letters folded to Latin, every run of whitespace (newlines too) one space.
func skeleton(cleaned string) string {
	d := norm.NFKD.String(cleaned)
	var b strings.Builder
	b.Grow(len(d))
	space := false
	for _, r := range d {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		r = unicode.ToLower(r)
		if l, ok := confusables[r]; ok {
			r = l
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ───────────────────────────── redaction ─────────────────────────────

func placeholder(kind string) string { return "[REDACTED_" + kind + "]" }

// Redact returns the text with personal data and credentials replaced by [REDACTED_<TYPE>] markers, and how many of each
// were replaced. The text is normalised first (see clean), so what comes back is the NFKC form of the input. It is
// idempotent: redacting redacted text changes nothing.
func (s *Shield) Redact(text string) (string, map[string]int) {
	counts := map[string]int{}
	t := clean(text)
	for _, re := range secretREs {
		t = re.ReplaceAllStringFunc(t, func(string) string { counts[TypeSecret]++; return placeholder(TypeSecret) })
	}
	t = emailRE.ReplaceAllStringFunc(t, func(string) string { counts[TypeEmail]++; return placeholder(TypeEmail) })
	t = panRE.ReplaceAllStringFunc(t, func(string) string { counts[TypePAN]++; return placeholder(TypePAN) })
	t = redactNumbers(t, counts)
	return t, counts
}

type group struct{ s, e int } // a run of ASCII digits, as byte offsets

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// redactNumbers finds cards, Aadhaar numbers and Indian mobile numbers. They are digit groups joined by single spaces or
// hyphens, which a regular expression without lookaround cannot delimit properly, so this scans by hand. From every group
// that starts a number it takes the LONGEST run of following groups that forms a recognised number, then carries on after
// it. Trying every group as a start means "order 99 4532 0151 1283 0366" still finds the card.
func redactNumbers(text string, counts map[string]int) string {
	var out strings.Builder
	n, last, i := len(text), 0, 0
	for i < n {
		if !isDigit(text[i]) {
			i++
			continue
		}
		groups := chainGroups(text, i)
		k := 0
		for k < len(groups) {
			m, kind := bestNumber(text, groups, k)
			if m < 0 {
				k++
				continue
			}
			start := groups[k].s
			if kind == TypePhone && start > 0 && text[start-1] == '+' {
				start--
			}
			out.WriteString(text[last:start])
			out.WriteString(placeholder(kind))
			last = groups[m].e
			counts[kind]++
			k = m + 1
		}
		i = groups[len(groups)-1].e
	}
	out.WriteString(text[last:])
	return out.String()
}

// chainGroups returns the digit groups starting at i that are joined by a single space or hyphen.
func chainGroups(text string, i int) []group {
	var gs []group
	n := len(text)
	for {
		j := i
		for j < n && isDigit(text[j]) {
			j++
		}
		gs = append(gs, group{i, j})
		if j+1 < n && (text[j] == ' ' || text[j] == '-') && isDigit(text[j+1]) {
			i = j + 1
			continue
		}
		return gs
	}
}

// bestNumber returns the last group index of the longest recognised number starting at group k, and its type; -1 if none.
// At most 19 digits are ever considered, which also bounds the work per start.
func bestNumber(text string, groups []group, k int) (int, string) {
	var digits [19]byte
	var lens [19]int
	nd, ng := 0, 0
	bestM, bestKind, bestRank := -1, "", 0
	for m := k; m < len(groups); m++ {
		g := groups[m]
		gl := g.e - g.s
		if nd+gl > len(digits) || ng >= len(lens) {
			break
		}
		copy(digits[nd:], text[g.s:g.e])
		nd += gl
		lens[ng] = gl
		ng++
		kind, rank := classify(lens[:ng], digits[:nd])
		if kind != "" && (m > bestM || (m == bestM && rank > bestRank)) {
			bestM, bestKind, bestRank = m, kind, rank
		}
	}
	return bestM, bestKind
}

func classify(lens []int, d []byte) (string, int) {
	if n := len(d); n >= 13 && n <= 19 && luhn(d) {
		return TypeCreditCard, 3
	}
	if isPhone(lens, d) {
		return TypePhone, 2
	}
	if len(d) == 12 && d[0] >= '2' && (shape(lens, 12) || shape(lens, 4, 4, 4)) {
		return TypeAadhaar, 1
	}
	return "", 0
}

func shape(lens []int, want ...int) bool {
	if len(lens) != len(want) {
		return false
	}
	for i := range lens {
		if lens[i] != want[i] {
			return false
		}
	}
	return true
}

func mobileStart(b byte) bool { return b >= '6' && b <= '9' }

// isPhone recognises Indian mobile numbers: ten digits starting 6-9, written whole or as 5+5, optionally after +91, 91, 0
// or 0091 (glued or as a separate group).
func isPhone(lens []int, d []byte) bool {
	str := string(d)
	switch {
	case shape(lens, 10), shape(lens, 5, 5):
		return mobileStart(d[0])
	case shape(lens, 11):
		return d[0] == '0' && mobileStart(d[1])
	case shape(lens, 12):
		return str[:2] == "91" && mobileStart(d[2])
	case shape(lens, 14):
		return str[:4] == "0091" && mobileStart(d[4])
	case shape(lens, 2, 10), shape(lens, 2, 5, 5):
		return str[:2] == "91" && mobileStart(d[2])
	case shape(lens, 1, 10), shape(lens, 1, 5, 5):
		return d[0] == '0' && mobileStart(d[1])
	case shape(lens, 4, 10), shape(lens, 4, 5, 5):
		return str[:4] == "0091" && mobileStart(d[4])
	}
	return false
}

// luhn reports whether the digits pass the Luhn checksum (all payment cards do; most random numbers do not).
func luhn(d []byte) bool {
	sum, alt := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		v := int(d[i] - '0')
		if alt {
			v *= 2
			if v > 9 {
				v -= 9
			}
		}
		sum += v
		alt = !alt
	}
	return sum%10 == 0
}

// ───────────────────────────── inspection ─────────────────────────────

func detectInjection(cleaned string) bool {
	flat := skeleton(cleaned)
	// One scan, of the skeleton: it matches everything the text as given or as normalised would (the patterns are
	// case-insensitive and the skeleton only removes marks and extra spaces and folds look-alikes), and more.
	if injectionRE.MatchString(flat) {
		return true
	}
	for _, line := range strings.FieldsFunc(cleaned, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if roleLineRE.MatchString(skeleton(line)) || roleLineRE.MatchString(line) {
			return true
		}
	}
	return false
}

func orderedTypes(counts map[string]int) []string {
	var out []string
	for _, t := range typeOrder {
		if counts[t] > 0 {
			out = append(out, t)
		}
	}
	return out
}

// Inspect redacts, screens and quarantines content that came from origin (for example "shopify" or "email"; anything outside
// letters, digits, "_", "." and "-" becomes "_", and it is cut to 64 characters). It returns ErrTooLarge for input over
// MaxInputRunes and otherwise never fails.
func (s *Shield) Inspect(content, origin string) (Result, error) {
	if utf8.RuneCountInString(content) > MaxInputRunes {
		return Result{}, ErrTooLarge
	}
	if origin == "" {
		origin = "unknown"
	}
	origin = originUnsafeRE.ReplaceAllString(origin, "_")
	if r := []rune(origin); len(r) > 64 {
		origin = string(r[:64])
	}

	cleaned := clean(content)
	redacted, counts := s.Redact(cleaned)
	injection := detectInjection(cleaned)

	body := Withheld
	if !injection {
		// Neutralise markup so the content cannot close the quarantine tag.
		body = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(redacted)
	}
	return Result{
		Safe:      !injection,
		Injection: injection,
		Text:      `<untrusted_context origin="` + origin + "\">\n" + body + "\n</untrusted_context>",
		PII:       orderedTypes(counts),
		Counts:    counts,
	}, nil
}

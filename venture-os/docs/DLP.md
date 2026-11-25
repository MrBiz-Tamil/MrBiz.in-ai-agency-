# Privacy filter (`internal/dlp`)

A Go port of the removed ABOS prototype's `ai-service/security/dlp_shield.py` (see git commit `6198fcd`), with the same
output format and several fixes. It
cleans text that comes from outside the business before an agent or a log sees it.

**Status: the package is written and tested. It is not yet called by anything.** Where to connect it is a decision, set out in
the last section.

## What it does

```go
shield := dlp.New()
res, err := shield.Inspect(customerEmailBody, "email")
// res.Text      -> put this in a prompt
// res.Safe      -> false if an injection phrase was found (the text is then withheld)
// res.PII       -> ["EMAIL", "PHONE_IN"]: which kinds of data were redacted, never the data
// res.Counts    -> {"EMAIL": 1, "PHONE_IN": 2}

clean, counts := shield.Redact(text) // redaction only, no quarantine: for logs
```

1. **Redacts** (each replaced by `[REDACTED_<TYPE>]`):

   | Type | What |
   | --- | --- |
   | `CREDIT_CARD` | 13-19 digits that pass the Luhn checksum, in any spacing: groups joined by up to three of space, tab, line break, `- . , / | _` or a Unicode dash (`4111 1111...`, `4111 - 1111...`, `4111.1111...`, one per line, glued) |
   | `AADHAAR` | 12 digits starting 2-9, whole or as 4-4-4 |
   | `PAN_IN` | five capitals, four digits, one capital |
   | `EMAIL` | the usual address shape |
   | `PHONE_IN` | Indian mobile numbers: 10 digits starting 6-9, whole or 5+5, with or without `+91`, `91`, `0`, `0091` |
   | `SECRET` | API keys (Anthropic, OpenAI, Stripe, Razorpay, AWS, Google, GitHub, Slack), JWTs, bearer tokens, private keys, and `password:`, `token=`, `api_key:` style assignments |

2. **Screens for prompt injection** ("ignore previous instructions", "system prompt", role lines such as `system:`, `<script>`,
   and similar). Hostile text is **withheld entirely** (`[CONTENT WITHHELD: possible prompt injection]`), not passed on.
   Redaction types are still reported, so the audit trail shows what the message contained.
3. **Quarantines** what remains in `<untrusted_context origin="...">...</untrusted_context>`, with `& < >` escaped so the text
   cannot close the block or open another. The origin is cut to 64 characters and anything outside letters, digits, `_`, `.`
   and `-` becomes `_`.

Input over 100,000 characters is refused (`ErrTooLarge`), not truncated: a partial scan would let the unscanned part through.

## What is different from the Python original

| Change | Why |
| --- | --- |
| **Detection runs after Unicode normalisation.** Full-width characters, zero-width and other invisible characters are handled, and digits of any script (Devanagari, Arabic-Indic, full-width) count as digits. | The original redacted the raw text, so `९८७६५४३२१०` or `9876<zero-width>543210` passed straight through. |
| **Injection screening reads invisible characters two ways.** Once deleted (`ig<ZWSP>nore`) and once as a space (`ignore<ZWSP>previous`); a match under either reading withholds the text. Hangul fillers, the braille blank and combining marks count as gaps too. | Deleting alone turned `ignore<ZWSP>previous instructions` into `ignoreprevious instructions`, which matched nothing. |
| **Number groups may be separated by up to three separator characters**, not only a lone space or hyphen, and a phone prefix may be glued to the first half (`098765 43210`). | Cards written with a double space, `" - "`, dots, commas or one group per line, and the glued-prefix phone form, were not redacted. |
| **Phone numbers in the formats people really type**: `98765 43210`, `91 9876543210`, `919876543210`, `+91-98765-43210`. | The original needed ten unbroken digits, and `91 ...` without a plus slipped by. |
| **A card is found even with other digits before or after it** (`order 99 4532 0151 1283 0366`). Every group is tried as a start, longest number wins. | The original took the whole chain, failed the checksum on it, and left the real card in the text. |
| **Look-alike letters** (Cyrillic and Greek) are folded to Latin and accents dropped before injection matching. | `ignоre` with a Cyrillic `о` evaded every phrase. |
| **Role lines are matched per line** and also after folding. | The original flattened newlines, so it relied on a raw match. |
| **`SECRET` is a new type.** | Credentials pasted into a message are the worst thing an agent can repeat. |
| **A cut-off private key** is redacted with the key material after its header. | Found by testing: only the header line was removed at first. |
| The twelve injection phrases are one pattern, scanned once. | About twice as fast. |

Kept deliberately: the PAN pattern is loose (any five capitals, four digits, one capital), because sample PANs break the strict
rule and over-redacting is the safe error. A grouped 16-digit number that fails the checksum is never called a card, but its
last twelve digits can look like an Aadhaar number and are redacted, as in the original.

## What it does NOT do

* **It is a heuristic.** It lowers risk; it cannot prove text is harmless. An injection phrased in a way no pattern knows is not
  caught, and neither is one written with spaces between letters (`i g n o r e`), leet-speak, or another language.
* **Only the listed kinds of data are redacted.** Names, street addresses, bank account numbers, IFSC codes, UPI ids, GST numbers,
  passport and voter numbers, dates of birth and anything in an image or attachment are not.
* **A number that is not in a recognised shape is left alone**, for example a mobile number written with dots or words.
* **It does not replace the controls that matter:** agents have no tool that moves money without a human approval, tools take no
  tenant argument, and the approval screen shows the exact payload. Treat this filter as one layer.

## How well it is tested

* 23 tests and a fuzz target, including every privacy-filter case from the Python suite, the formats above, evasions, a property test that 3,000
  random valid cards in random spacing and context are always hidden, and one that 500 random ordinary 16-digit numbers are never
  touched.
* **Fuzzing**: more than 900,000 arbitrary inputs over three runs, checking that it never panics, always returns a well-formed
  wrapper with exactly two `<` characters, and that redacting twice changes nothing more. It found no failures. (The "stable on a
  second pass" rule exists because normalising before removing invisible characters leaves text that still changes; that was
  worked out by reasoning and is covered by a test and by a planted-bug check, not found by the fuzzer.)
* **Planted bugs**: 22 deliberate bugs (checksum steps, length limits, separators, prefixes, normalisation, escaping, limits, the
  withholding rule, look-alike folding) were each caught by a test. One more (a length check that the digit buffer already
  enforces) changes nothing. One survivor exposed a missing test, now added (a chat-turn marker hidden with look-alike letters);
  three others showed code that was redundant (a first normalisation pass and two scans of text the skeleton already covers), so I
  removed it.
* **Speed**: about 0.6 ms for a 1 KB message; about 50-95 ms for the 100,000-character maximum, including hostile shapes (a
  100,000-digit run, 50,000 one-digit groups). These are timings on this machine, not a guarantee.
* Not tested: real customer data, other languages' phrasing, and the race detector (this machine has no C toolchain for it; a
  `Shield` has no mutable state, so there is nothing to race on).

## Where to connect it (a decision, not yet done)

Redacting everything everywhere would break the product: an agent that handles a customer needs that customer's phone number and
e-mail from the CRM. So:

| Text | Apply `Inspect`? | Reason |
| --- | --- | --- |
| Free text typed by an outsider that reaches an agent: CRM notes, activity bodies, task descriptions, support messages, webhook bodies, Notion page text read back | **Yes** | This is where injection and pasted secrets arrive |
| Structured CRM fields (a contact's phone and e-mail) | **No** (injection screen only, if at all) | The agent legitimately needs them |
| Agent tasks created from outsider text (`untrusted = true` in the work-queue design) | **Yes**, on the goal before it is stored | Matches the design's "data, not instructions" rule |
| Logs and error messages | **`Redact`** | No quarantine wrapper needed |
| The founder's own messages to Genesis | No | Trusted, and the founder expects the model to see them |

The first real use is the MCP bus's read tools for notes and activities and the work queue's untrusted goals. Wiring it there
changes what agents see, so it needs the product decision above and a look at real tool results.

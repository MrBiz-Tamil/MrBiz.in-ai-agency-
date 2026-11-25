# Evaluation: claims versus what is built

This replaces the pros-and-cons text that was circulating for "Mrbiz.in / ABOS". That text mixed what exists with what
is planned. This document separates them. Every row says **Built**, **Partly built** or **Roadmap**, and says how it
was checked.

Two code bases are involved and they must not be confused:

* **ABOS** (`backend-core`, `ai-service`, `frontend`): the older code. It was read, not changed, and its tests were not run.
* **Venture OS** (`venture-os/`): the newer code, where all of the fixes below were made.

How to read "tested": almost everything here is tested against **in-process fakes** of the outside world (a fake Google,
a fake Zoom, a fake Notion, fake speech and telephony), or against PostgreSQL through PGlite. A fake that I wrote from
documentation proves my code does what I understood the documentation to say. It does not prove the real provider
agrees. No real provider account, real Postgres cluster in production, real telephony or real model was used for the
connector, voice or agent claims. Each row says so where it matters.

## 1. Claims about ABOS that the code did not support

Found by reading the ABOS code. "I found no X" means a search came up empty; treat it as strong evidence, not proof.

| Claim | What the code showed | Venture OS now |
| --- | --- | --- |
| "28-decimal fixed-point" money | Decimals are arbitrary precision (`shopspring/decimal`, banker's rounding); columns are `NUMERIC(10,2)`, `(12,6)`, `(18,4)`. Nothing is 28-decimal. | Money is integer minor units plus a currency, end to end. No floats, no rounding on input (an invalid amount is rejected). |
| Spending caps per hour and per day | A **daily** cap per agent on **LLM cost in USD**. It does not cap business money and has no hourly limit. | **Built.** Hard caps per payment, per hour and per day, per currency, on payments and purchase orders. Enforced inside the approval transaction, so even a founder cannot approve past a cap (section 2). |
| Policy rules | Two defaults only: refund over 1,000 rupees, discount over 12 percent. No payment or purchase-order rule. | Payments are always high risk; purchase orders and payments are capped; anything that emails other people needs an approval (section 2). |
| "Decision X-Ray" is a cryptographic trail | Append-only (triggers block update, delete, truncate) but no hash chain or signature; rows lack model tier, goal and approval time. | **Partly built.** Each tenant's audit log is a hash chain that detects an edited or removed entry anywhere in the middle. It cannot detect deletion of the **newest** rows unless you keep the head fingerprint somewhere the database administrator cannot write. The Settings screen shows the head to copy. Model tier and goal text are not recorded. |
| Voice agent with sub-800 ms cart recovery | ABOS has a 121-line text handler. `voice_orchestrator.py` says no speech pipeline exists and returns UNIMPLEMENTED outside dev. No speech-to-text, text-to-speech, telephony or cart-abandonment trigger. | **Partly built, and the claim stays unproven.** The call-handling core exists and is tested (section 3). There is **no** speech, voice or telephony provider and no cart-abandonment trigger. No real latency has ever been measured. |
| Stock balanced across Amazon and Shopify in 50 ms | The PRD's webhook target is 250 ms at P99; 50 ms is a Redis read target. The inventory agent runs every 15 minutes. No Amazon code. | **Roadmap.** No commerce connector exists in either code base. |
| Latency numbers in general | Every figure is a target in the PRD or technical spec. No benchmark or measurement. | The voice core measures time to first audio per turn and flags turns over budget, but only against fakes. No measured figure is claimed. |
| Connectors for Shopify, Amazon, Odoo, ERPNext, Razorpay, Shiprocket | None in ABOS. The MCP gateway can route to external MCP servers a workspace registers; none are in the repo. No rate limiting or backoff. | Venture OS has ERPNext, Keycloak and Hermes adapters (earlier work) and now Google Calendar, Google Meet, Zoom and Notion (section 2), all on a shared client with rate limiting, retries and a circuit breaker. Shopify, Amazon SP-API, Razorpay, Shiprocket and Odoo are **Roadmap**. |
| "8 agents run operations 24/7" | The roster is seeded data. `DecomposeGoal` and `StreamReasoning` are canned dev scripts. Real LLM-backed pieces are the policy compiler and the router. | Not claimed. Agents act through MCP tools; no agent loop has been run with a real model. |
| "Zero financial hallucination risk", Zap logging | "Zero risk" is an overclaim; Zap is not in `go.mod`. | The honest statement: agents cannot author money numbers (prices come only from the pricing engine, and money-moving requests need a human decision bound to the payload hash). That reduces the risk; it does not make it zero. |
| Kernel versus an ERP "double-entry" | ABOS migrations have no ledger or journal; the kernel computes totals but keeps no books. | Venture OS posts balanced journals when an invoice is approved (Dr receivable, Cr revenue, Cr tax payable). It is still not a full ERP. |

What did hold up in ABOS: the language and framework versions, the named files, exact-decimal order math, Redis plus
database idempotency, the atomic budget circuit breaker (for LLM cost), fail-closed approval policy, append-only audit
triggers, the agent roster, the two-tier model router with fallback, the data-loss-prevention filter, and webhook
signature plus de-duplication.

## 2. What was added in Venture OS, and how far to trust it

| Area | State | Checked how | Not proven |
| --- | --- | --- | --- |
| Reliable outbound HTTP (`internal/httpx`) | Built | Unit tests with a fake clock: rate limit, `Retry-After`, retries only when safe (idempotent methods or marked idempotent; 429 always), circuit breaker, no cross-host redirects, bounded bodies, scrubbed errors. Mutation-tested. | Behaviour under real provider outages. |
| Webhook verification (`internal/webhook`) | Built | HMAC check, replay window, de-duplication; mutation-tested. | No webhook endpoint is exposed yet. |
| Per-tenant secret vault (`internal/vault`) | Built | AES-256-GCM, bound to tenant and name, key versioning; a sealed secret moved to another tenant fails to open. | Key rotation operations and an external key manager. |
| Google Calendar and Meet, Zoom, Notion adapters | Built | Strict in-process fakes of each API (token expiry, revocation, rate limits, failures). Credentials are checked with the provider before they are stored. | **No real Google, Zoom or Notion account was used.** The Google consent screen (OAuth sign-in for a founder) is not built: you supply a refresh token. |
| Agent tools `meetings_ops`, `notes_ops` | Built | Scripted calls through the MCP bus, the control plane and PostgreSQL with row-level security, including tenant isolation. | How a real model uses them. |
| Invitations need approval | Built | Anything that makes Google email other people queues a `MEETING` approval; the executor runs after a human approves and is safe to retry. Zoom's API sends no invitations. | A real invitation was never sent. |
| Duplicate protection for external writes | Built | Platform-level idempotency for Zoom and Notion creates; Google events use a deterministic id; an unknown outcome is never silently repeated. | |
| Hard spend caps | Built | Checked in the approval transaction under a per-tenant lock; boundary and rejected-spend cases covered after mutation testing found them untested. Run on a real PostgreSQL 16: removing the lock makes `TestConcurrentApprovalsCannotSlipPastACapTogether` fail. | The test database most developers use (PGlite) has one connection and cannot show the race; it needs a real server. Not run on PostgreSQL 15 or 17. |
| Tamper-evident audit | Built, with a stated limit | Trigger-maintained hash chain; verification endpoint and Settings screen; tampering tests. | Truncation of the newest rows (above). |
| Settings screen (connections, spend limits, audit check) | Built | Browser check against the dev stack with fake providers; unit tests. | Not run against real providers or real Keycloak. |

Also changed: `db/schema_controls.sql` is schema version 3 and `db/schema_rag.sql` is now version 4. `db/apply.sql` is still
not run by any test (no `psql` was available), and pgvector is still untested.

## 3. Voice: what exists and what does not

Built (`internal/voice`): a call session that takes a speech recognizer, a speech synthesizer, a phone line and a
"responder" as plain interfaces; passes only final, non-blank, length-bounded utterances to the responder; stops talking
the moment the caller speaks (barge-in) and flushes the line; replaces an answer still being spoken when a new
utterance arrives; measures time to first audio per turn against an 800 ms budget and records percentiles; plays a
short filler when the responder is slow; turns a failing, slow or empty responder into an apology and a handoff to a
person; speaks a goodbye in full before hanging up; enforces turn and call-length limits; and does not leak goroutines.
It carries `Verified: false` for every caller unless the telephony layer proves identity, because caller ID can be spoofed.
Tests use short real delays against fakes, and seven mutations of the core (budget check, partial-transcript handling,
turn limit, length bound, barge-in flag, replace-on-new-utterance, handoff reason) were each caught.

Not built: any real speech-to-text or text-to-speech provider, any telephony or WebRTC integration, a cart-abandonment
trigger, calling-hours and consent rules, and a connection from the responder to the agent tools. The race detector was
not run because this machine has no C toolchain enabled for it. **No claim about real-world call latency is made or
supportable until a real provider is wired in and measured.**

## 4. Corrected wording

Use this in place of the earlier pros-and-cons text:

* Venture OS is a multi-tenant, approval-gated business operating system. Money-moving actions need a human decision
  bound to the exact payload, are subject to hard per-payment, hourly and daily caps per currency, and are recorded in a
  tamper-evident audit chain.
* It connects to Google Calendar and Meet, Zoom and Notion through adapters that were tested against simulations of
  those services, not against the real ones. Commerce connectors (Shopify, Amazon, Razorpay, Shiprocket, Odoo) are planned.
* A voice call engine with barge-in, handoff and latency measurement exists; speech providers and telephony do not.
  Any latency figure is a target, not a measurement.
* Agents cannot invent prices; this lowers financial error risk and does not eliminate it.

## 5. What to do next, in order

1. Run the connector adapters once against real accounts (Google, Zoom, Notion) and fix what the simulations got wrong.
2. Exercise `db/apply.sql` with `psql` against a version-2 database (the cap race and audit tests already pass on a real PostgreSQL 16).
3. Anchor the audit head outside the database on a schedule.
4. Build the Google consent flow so founders do not paste refresh tokens.
5. Pick speech and telephony providers, wire them behind the existing interfaces, then measure latency for real.
6. Add commerce connectors one at a time, each against its sandbox, using the same client, vault and approval rules.

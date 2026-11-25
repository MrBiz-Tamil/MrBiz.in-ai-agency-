# Venture OS — Monorepo & Runtime Architecture

Venture OS is a multi-tenant, AI-native business operating system. A founder describes a business in
conversation; the platform designs the operating model, provisions an isolated workspace (`tenant_id`) across
ERP / CRM / Support / Agents, hires an AI org chart, and runs daily operations behind human approval gates.

> This directory is self-contained. The earlier ABOS prototype (`backend-core/`, `ai-service/`, `frontend/`) was removed
> from the repository; it remains in git history at commit `6198fcd`.

## 1. Monorepo layout

Directories marked **(step N)** are delivered by that step of the build plan; they are listed so the boundaries
are fixed now, but they are not created until real code exists for them.

```
venture-os/
├── docker-compose.yml          Full stack (infra always on; app services behind profiles)
├── docker-compose.dev.yml      Dev overlay: localhost-only DB/Redis ports, http scheme
├── .env.example                Every variable the stack reads; scripts/gen-env.* fills secrets
├── docs/
│   └── ARCHITECTURE.md         This file
├── infra/                      Config mounted into third-party containers
│   ├── postgres/init/          01-bootstrap.sh — roles, databases, extensions (first boot only)
│   ├── keycloak/               realm-venture.json — realm, clients, roles, founder user, Organizations on
│   ├── libredesk/              config.toml — secrets injected through LIBREDESK_* env overrides
│   ├── hermes/                 config.yaml — MCP client pointing at the tenant-scoped MCP bus
│   └── caddy/                  Caddyfile — single TLS edge, host-based routing
├── deploy/
│   ├── go.Dockerfile           One multi-stage build for every Go binary (ARG SERVICE=...)
│   └── web.Dockerfile          SvelteKit (adapter-node) image
├── scripts/                    gen-env.ps1 / gen-env.sh — generate secrets into .env
│
├── go.mod                      Single Go module: github.com/mrbiz/venture-os            (done)
├── proto/venture/{tenant,genesis,provisioner}/v1/   Protobuf contracts, buf-linted         (done)
├── gen/                        buf-generated Go + Connect stubs (committed; scripts/gen-proto.*) (done)
├── cmd/
│   ├── controlplane/           Tenants + Genesis + Provisioner RPC server (Finance, Governance: step 5) (done)
│   ├── mcpbus/                 Tenant-scoped MCP server (streamable HTTP)                (done)
│   └── skillgen/               CLI: blueprint.json → Hermes SKILL.md files               (step 4)
├── internal/
│   ├── genesis/                Interview engine: strict JSON envelope, validate-and-repair loop (done)
│   ├── skillgen/               Deterministic SKILL.md renderer + lint                    (done; cmd/skillgen CLI not written)
│   ├── provisioner/            Saga: 4 parallel stages, retries, reverse compensation, resume (done)
│   ├── store/                  Postgres store: tenants, blueprints, saga state, seeding   (done)
│   ├── mcptoken/               Per-tenant HS256 bearer tokens for the MCP bus            (done)
│   ├── connectors/anthropic/   Real ports.LLM on the Messages API                         (done)
│   ├── connectors/{keycloak,erpnext,hermes}/   Real adapters (Keycloak Organizations, ERPNext Company+CoA, Hermes profiles) (done)
│   ├── connectors/                  per-system real/fake selection with guard rails                 (done)
│   └── connectors/{twenty,libredesk}/  NOT written: see §9 for why
│   ├── financeengine/          Integer-money pricing (SaaS / D2C / EXIM), document totals, cash-flow forecast (done)
│   ├── controlplane/           also: AgentService (tools' backend), ApprovalService (founder inbox), executors (done)
│   ├── mcpbus/                 MCP JSON-RPC server, 9 typed tools + 14 crm_ops tools when a CRM is real, token auth, rate limiting (done)
│   ├── crmops/                 The 19 CRM operations defined once (14 for agents, 5 human-only): schema, policy, strict args, executor (done)
│   ├── ratelimit/              Redis fixed-window per-tenant limiter + in-memory twin    (done)
│   ├── testenv/                Whole control plane in-process for end-to-end tests       (done)
│   ├── crm/                    CRM interface (companies, contacts, deals, activities), shared validation (done)
│   │   ├── crmtest/            Contract suite every CRM backend must pass (16 behaviours)  (done)
│   │   ├── memory/             In-memory reference backend; the suite is debugged against it first (done)
│   │   └── builtin/            Venture OS's own CRM on PostgreSQL + the ports.CRM provisioner adapter (done)
│   ├── tenancy/                Scope in context + WithTx: applies SET LOCAL app.* for RLS (done)
│   ├── domain/                 Blueprint types + Validate() business rules, content hash (done)
│   ├── ports/                  Interfaces to every external system and the LLM          (done)
│   ├── fakes/                  Stateful in-memory ports: idempotent, tenant-scoped, fault injection (done)
│   ├── fixtures/               Realistic valid Blueprint (EXIM) shared by tests           (done)
│   ├── testdb/                 Real Postgres (PGlite or $VENTURE_TEST_DATABASE_URL) with schema applied (done)
│   ├── auth/                   OIDC bearer verification (JWKS) + interceptor             (done)
│   └── platform/               config, logging, pgx pool, redis, connect interceptors    (step 4)
├── db/
│   ├── schema.sql              Multi-tenant schema: RLS, idempotency keys, ledger, approvals (done, tested)
│   ├── schema_crm.sql          Built-in CRM tables (schema version 2), same RLS model (done, tested)
│   ├── schema_rag.sql          pgvector knowledge_chunks with the same RLS (version 3; done, NOT tested: needs pgvector)
│   ├── apply.sql               Incremental psql entrypoint used by compose service `schema`: applies each file whose
│   │                           schema_version row is missing (done; the psql script itself is NOT run in any test)
│   └── schema_test.go          Integration tests: isolation, ledger, approvals, blueprint freeze (done)
└── web/                        SvelteKit "Chairman Command Center"                       (step 6)
```

**One Go module, many binaries.** The control plane, MCP bus and skill generator share the Blueprint types, the
tenancy context and the proto stubs. A single module keeps those in lock-step; the Docker build selects the
binary with `ARG SERVICE`.

## 2. Runtime topology

```
                                 ┌────────────────────────── edge (public) ──────────────────────────┐
 browser ──► Caddy :80/:443 ──►  │ web  keycloak  twenty-server  libredesk  erp-frontend            │
                       │         │ controlplane (ConnectRPC, h2c)                                    │
                       │         └───────────────────────────────────────────────────────────────────┘
                       │
   ┌───────────────────┴─ agents (egress to LLM APIs only) ─┐      ┌─ data (internal, no egress) ─┐
   │ hermes ──MCP──► mcpbus ──ConnectRPC──► controlplane ────┼────► │ postgres (pgvector)  redis  │
   │                                                         │      └──────────────────────────────┘
   └─────────────────────────────────────────────────────────┘      ┌─ erp (internal) ─────────────┐
                                                                    │ mariadb, redis×2, workers    │
                                                                    └──────────────────────────────┘
```

Network zones are enforced by Docker, not convention:

| Network  | `internal` | Members | Why |
| --- | --- | --- | --- |
| `edge`   | no  | caddy, web, keycloak, controlplane, twenty, libredesk, erp-frontend | The only zone Caddy can reach. `web` calls `controlplane` here, server to server. |
| `data`   | **yes** | postgres, redis and the services that need them | Databases have no route to the internet. |
| `agents` | no  | hermes, mcpbus, controlplane | Hermes needs LLM egress; it is **not** on `data`. |
| `erp`    | **yes** | mariadb, erp redis ×2, ERPNext workers, erp-frontend, controlplane | ERPNext's own stores stay private. |
| `egress` | no  | twenty-worker, erp-backend, erp workers/scheduler | Outbound-only (email, webhooks, FX rates). Nothing listens here; it lets workers reach the internet while their data networks stay `internal`. |

Consequences worth stating plainly:

* **Agents cannot reach a database.** Hermes is attached to `agents` only, so "agents never write raw SQL" is a
  network fact. Its sole door to business data is the MCP bus.
* **The MCP bus holds no database credentials.** It verifies the per-tenant token, rate-limits per tenant (Redis) and
  forwards typed ConnectRPC calls to the control plane's `AgentService`, **passing the agent's own token through**.
  The control plane verifies that token again and takes the tenant from it; no request message has a tenant field.
  So even a compromised bus cannot act as a tenant it has no token for. All deterministic math (pricing, ledger,
  cash-flow) and all state transitions live in the control plane.
* **Paperclip was removed from the stack.** Agent work is queued and limited by Venture OS itself (see section 12a); there is no
  second place that defines who may do what.

## 3. Tenant isolation model

`tenant_id` (UUID) is the unit of isolation in every layer. A Keycloak **Organization** maps 1:1 to a tenant; the
token's `organization` claim is resolved to `tenant_id` by `internal/auth` and never read from a request body.

| Layer | Mechanism |
| --- | --- |
| PostgreSQL (`venture` db) | Every table carries `tenant_id NOT NULL`. `ENABLE` + `FORCE ROW LEVEL SECURITY` with a policy on `current_setting('app.tenant_id')`. The runtime role `venture_app` is `NOSUPERUSER NOBYPASSRLS`; only `venture_owner` (migrations) owns tables. Each request runs in a transaction that does `SET LOCAL app.tenant_id`. |
| Idempotency | `UNIQUE (tenant_id, idempotency_key)` on every mutating table; provisioner steps and approval executions are replayable. |
| Redis | Keys are `t:{tenant_id}:…`; streams are `t:{tenant_id}:events`. |
| Vector / RAG | pgvector in the `venture` db, rows carry `tenant_id` and fall under the same RLS policy. |
| MCP | Bearer token minted per tenant (and per agent). The bus derives `tenant_id` from the token; no tool accepts a tenant argument. |
| Agents | Each tenant gets its own Hermes profile + skills directory (no separate orchestration product; see section 12a); Hermes tool allow-lists are generated from the blueprint. |
| CRM (built-in) | `crm_*` tables in the same database: `tenant_id`, RLS, composite `(tenant_id, id)` foreign keys, per-tenant idempotency keys (§10). |
| LibreDesk / ERPNext | One LibreDesk inbox set, one ERPNext **Company** per tenant (ERPNext site-per-tenant is the upgrade path for stricter isolation — see §6). |

Postgres roles created by `infra/postgres/init/01-bootstrap.sh`:

| Role | Used by | Notes |
| --- | --- | --- |
| `postgres` | bootstrap only | Superuser; not handed to any service. |
| `venture_owner` | schema job | Owns tables; migrations run as this role. |
| `venture_app` | controlplane | `NOBYPASSRLS`; DML only. |
| `keycloak`, `twenty`, `libredesk` | the matching third-party app | Each owns exactly one database. |

## 4. Control flows

**Conversational company creation**
1. Founder chats in the wizard (`web`) → `GenesisService.Converse` (unary ConnectRPC; streaming can be added later) in the control plane.
2. The control plane drives the interview with an LLM, accumulating a draft `CompanyBlueprint`; every turn is
   re-validated against the JSON Schema + business rules (chart of accounts balances, margin floors ≥ 0, …).
   A blueprint is also refused (and, in the interview, sent back to the model to repair) when any free-text value is not
   one plain line within its length limit (`domain.Max*`), or when its agents' skills could not be generated
   (`skillgen.Check`, which counts bytes). The text is copied into each agent's instruction file, so a line break could
   otherwise forge a section of it, and a name over the database's 200 characters would fail only at approval.
3. On founder approval the blueprint is frozen (content-hashed, stored in `company_blueprints`).
4. `ProvisionerService.Provision` runs a saga — Keycloak org → Postgres rows → ERPNext company/CoA → CRM
   pipelines (verified against the built-in CRM) → LibreDesk inboxes/KB → Hermes profile + `SKILL.md` files — each
   step idempotent (`provision_steps`) with a compensating action, so a partial failure rolls back cleanly.

**Agent action under governance**
1. Hermes calls an MCP tool (e.g. `erp_pricing_and_ledger.create_invoice`) with its tenant token.
2. The bus validates the token, rate-limits, and calls the control plane over ConnectRPC.
3. High-stakes tools never execute inline: the control plane writes an `approval_queue` row (`PENDING_APPROVAL`)
   and returns its id. The tool result tells the agent the action is awaiting the founder.
4. Founder approves in the Command Center → the control plane executes the stored, hash-verified payload exactly
   once (compare-and-set on status) and appends to `audit_logs`.

## 5. Port / host map (default `BASE_DOMAIN=localhost`)

| Host | Service | Published? |
| --- | --- | --- |
| `app.localhost` | web (SvelteKit) | via Caddy |
| `auth.localhost` | Keycloak | via Caddy |
| `crm.localhost` | Twenty | via Caddy |
| `support.localhost` | LibreDesk | via Caddy |
| `erp.localhost` | ERPNext | via Caddy |
| — | controlplane, mcpbus, hermes, postgres, redis | **never** published (dev overlay publishes postgres/redis on 127.0.0.1 only) |

Containers resolve the public hostnames through Docker network aliases on Caddy, so OIDC issuer URLs are
identical inside and outside the stack.

## 6. Known design decisions and open items

* **Hermes is built from source.** The upstream project publishes no versioned image that I could
  verify, so compose builds it from a pinned git ref (`HERMES_REF`). First build is slow; pin
  and mirror the images to your own registry for production.
* **Four of the five systems have a real adapter** (Keycloak, ERPNext, the built-in CRM, Hermes); see §9. Only support
  (LibreDesk) is simulated, and the control plane refuses to start with a simulated system unless that is explicit.
* **Hermes wiring.** Which Hermes profile does which work is decided by the agent work queue (section 12a), not by an
  orchestration product. Compose only guarantees Hermes can reach the MCP bus on `agents`.
* **Per-tenant ERPNext sites.** Compose boots one default site (`ERP_SITE_NAME`) and the nginx frontend routes by
  `Host`. Creating a site per tenant needs `bench new-site` access; the provisioner will do that through a small
  job-runner service decided in step 4. Until then tenants are separated by ERPNext Company.
* **Third-party API credentials** for Twenty, LibreDesk and ERPNext are created inside those products
  after first boot and pasted into `.env` (`*_API_KEY`); they cannot be known at compose time.
* **Keycloak admin console** is reachable at `auth.<domain>/admin` through Caddy. For production, block it at the
  edge or give it a separate admin hostname (`KC_HOSTNAME_ADMIN`).
* **Redis is a single instance** with one password and key prefixes. Per-service ACL users are the next hardening
  step.

## 7. MCP tools (Step 5)

| Tool | Family | Effect |
| --- | --- | --- |
| `list_pricing_rules`, `calculate_price`, `forecast_cash_flow` | `erp_pricing_and_ledger` | Read-only. Numbers come from `internal/financeengine`. |
| `create_quote`, `draft_invoice`, `draft_purchase_order` | `erp_pricing_and_ledger` | Compute, then queue a `PENDING_APPROVAL` item. Nothing is sent or posted. |
| `request_human_approval` | `governance` | Free-form request for PAYMENT, CAMPAIGN, AGENT_SKILL, OTHER. Cannot create quotes/invoices/POs, so agents cannot author money numbers. PAYMENT is always HIGH risk. |
| `escalate_blocker`, `get_approval_status` | `governance` | Tell the founder / read back a decision. |
| `crm_pipelines`, `crm_find_companies`, `crm_get_company`, `crm_find_contacts`, `crm_find_deals`, `crm_get_deal`, `crm_open_tasks` | `crm_ops` | Read-only. Results carry customer-typed text; the descriptions tell the agent to treat it as data, never as instructions. |
| `crm_create_company`, `crm_create_contact`, `crm_create_deal`, `crm_update_deal`, `crm_move_deal`, `crm_add_activity`, `crm_complete_task` | `crm_ops` | Direct writes to the business's own CRM (see below), attributed to `agent:<key>` and audited. |

**`crm_ops` policy** (`internal/crmops`, one definition used by both the bus and the control plane). The CRM writes need
no founder approval because each is an internal, reversible bookkeeping change and nothing leaves the business: logging
an `EMAIL` activity records an email, it does not send one (sending stays behind `request_human_approval`). Deliberately
absent for agents: archive, owner assignment, and editing a company's or contact's identity. Those five operations
(`crm_update_company`, `crm_update_contact`, `crm_archive_company|contact|deal`) are **human-only**: the bus never lists
them and the agent RPC answers "unknown CRM operation", and a test pins the exact set so a new operation cannot quietly
widen it. There is also no tenant argument anywhere (unknown arguments are rejected). A deal's amount is a pipeline estimate attributed to the agent, not a quote; customer prices
still come only from the pricing engine. Updates and moves accept `expected_version`, so an agent working from stale
data gets "the record changed since you read it" instead of overwriting a colleague. The tools are listed only when the
CRM is real (`VENTURE_CONNECTOR_CRM=real`); with a simulated CRM they are absent from `tools/list`, not faked.

**Not built:** `market_research` and `support_ops` need real backends (a research source, LibreDesk). They are absent from `tools/list` rather than faked, so an agent is never offered a tool that pretends.
Skills still list those families for agents granted them; until the backends exist the tools simply are not there.

**Approval execution** (`internal/controlplane/approval.go`): approving requires the payload hash the founder saw;
the decision and the claim to execute are compare-and-set, so concurrent clicks execute once. Executors re-verify
document totals from their lines before acting.

| Kind | On approval |
| --- | --- |
| `INVOICE` | Posts Dr receivable / Cr revenue / Cr tax payable using the blueprint's `system_accounts`; schedules the receipt. |
| `PURCHASE_ORDER` | Schedules the payment as a committed outflow (a commitment, not yet a liability: no journal). |
| `QUOTE` | Recorded as released. Delivery needs a support-channel connector that does not exist, and the result says so. |
| `MEETING` | Runs the queued calendar or meeting operation that would email other people (§11), with request id `approval:<id>` so a retry cannot duplicate it. |
| `PAYMENT`, `CAMPAIGN`, `AGENT_SKILL`, `OTHER` | Stay `APPROVED`: no executor, a human completes them. Nothing pretends to have paid. Approving a `PAYMENT` or `PURCHASE_ORDER` is first checked against the business's spend caps (§12). |

**Known gaps:** per-agent tool allow-lists are not enforced (the provisioner mints one token per tenant profile, so
all of a tenant's agents share it); a crash between executing and recording leaves an approval `EXECUTING` (side
effects are idempotent, but no janitor retries it yet); compliance deadlines are not modelled; ledger and forecast
are single-currency (the business's base currency).

## 8. Command Center (Step 6)

SvelteKit 2 / Svelte 5 / Tailwind 4, adapter-node. It is a **backend-for-frontend**: the browser holds only an
AES-256-GCM sealed, httpOnly, SameSite=Lax cookie (chunked across cookies; absolute 8 h lifetime). The OIDC
authorization-code + PKCE flow, token refresh and every ConnectRPC call run on the server, so no access or refresh
token ever appears in page data or JavaScript. State-changing requests also require a same-origin `Origin`, and
production responses carry a strict CSP, `X-Frame-Options: DENY` and `no-store`.

| Route | What it does |
| --- | --- |
| `/` | Portfolio: per-business status, cash, 90-day runway, pending approvals. Money is never summed across currencies. |
| `/launch` | "Explain & Launch": chat with the Venture Architect, live blueprint preview, review, approve-by-hash, launch with live provisioning progress. Resumable per business (`?b=`). |
| `/approvals`, `/approvals/[tenant]/[id]` | Global inbox across businesses (j/k/Enter); detail with engine-computed numbers; approve/reject bound to the payload hash; extra confirmation for payments and flagged items. |
| `/b/[tenant]` | One business: cash, runway, its approvals, provisioning steps, and a link to Sales. |
| `/b/[tenant]/crm` | **Sales** (the built-in CRM, §10). Pipeline board: one column per blueprint stage with per-currency totals; move a deal with the card's stage selector; new deal (with the pipeline's custom fields); `?p=` pipeline, `?q=` search. |
| `/b/[tenant]/crm/deals/[id]` | A deal: details, stage history (who moved it), notes/calls/meetings/tasks, versioned edit. |
| `/b/[tenant]/crm/companies`, `.../companies/[id]` | Company search and create; a company with its contacts, deals and open tasks; add contact. |
| `/b/[tenant]/crm/tasks` | Every open task in the business, with Done. |
| `Ctrl/Cmd+K` | Palette: pages, businesses, pending approvals, sign out. The portfolio switcher filters the inbox. |

Local development without Docker, Keycloak or an API key: see README ("Try the Command Center").

## 9. Connectors (real adapters)

Selection is per system (`VENTURE_CONNECTORS` default, `VENTURE_CONNECTOR_<SYSTEM>` override). Asking for `real` on a
system without an adapter is a startup error that explains why; outside `VENTURE_ENV=dev`, any simulated system needs
`VENTURE_ALLOW_FAKE_CONNECTORS=true`; every simulated system is logged at WARN and named in the UI banner.

| System | Adapter | What it does | Verified how |
| --- | --- | --- | --- |
| Identity (Keycloak) | `connectors/keycloak` | One Organization per tenant, found by its `tenant_id` **attribute** (never by name/alias), founder added as member; delete refuses any org that is not the tenant's. Service-account client credentials, cached token, one retry on 401. | Endpoints and status codes checked against Keycloak's published OpenAPI spec; tested against an in-process fake. **Not run against a real Keycloak.** |
| ERP (ERPNext) | `connectors/erpnext` | One Company per tenant on the shared site (marked in `company_description`), created from the standard template, then the blueprint's chart of accounts beneath the template roots with ERPNext account types set for bank/receivable/payable/tax. Resumable after a partial failure. | Company flow checked against ERPNext's `company.py`; the REST conventions (token auth, Host-routed site, `{"data"}` envelope, JSON filters, 409 duplicates) are from Frappe's documented API; tested against an in-process fake that I wrote from that understanding. **Not run against a real ERPNext**, and not covered: tax templates, deleting a company that already has transactions. |
| Agent runtime (Hermes) | `connectors/hermes` | Writes `profiles/venture-<slug>/` (config.yaml with the MCP URL, `.env` with the tenant token at 0600, SOUL.md, `skills/venture/<skill>/SKILL.md`) into the shared volume, atomically, through `os.Root`. Skills are verified against their approved SHA-256 before install; stale skills are removed; delete refuses unmarked directories. | Layout checked against Hermes' profile and skill docs; fully tested on a real filesystem, including through Postgres provisioning and rollback. **Not run with a live Hermes gateway.** |
| CRM | `crm/builtin` (built in) | The default. Nothing external to create: provisioning **verifies** that the pipelines `db.seed` wrote match the approved blueprint (names, stage order, win odds, custom fields) and fails if not; rollback never deletes CRM data. External CRMs plug in behind the same interface (§10). | Full contract suite + end-to-end provisioning on real PostgreSQL (PGlite); see §10 for what that does not prove. |
| Support (LibreDesk) | none | Simulated. LibreDesk is **single-tenant**: one set of users, teams and inboxes per instance, and creating an email or WhatsApp inbox needs real channel credentials. Isolation would require an instance per tenant (a provisioner-managed container), which changes the compose design. | |
| Orchestration | removed | **Paperclip was removed from the stack and from provisioning** (the port, its fake, the two provisioning steps and the database column are gone; it needed a bridge service and had no documented service credential, and it would have duplicated the approval and audit rules). A database created before this change keeps the old `paperclip` value in a check constraint and an unused `paperclip_ref` column, which is harmless. Reasons recorded: its API documents companies and agents, but board operations authenticate with browser sessions (no documented service credential), and it has no Hermes adapter type, so a bridge service (Paperclip `http` webhook to the Hermes API server, with Paperclip callbacks) is needed. | |

The designs for the two blocked connectors (Twenty, LibreDesk) are in [CONNECTORS-DESIGN.md](CONNECTORS-DESIGN.md): what the
source shows, the recommended approach, the decisions needed, and the spikes that come before any adapter code.

Operational notes: the control plane and Hermes must run with the same uid (`HERMES_UID`, default 10001) because the
control plane writes into the Hermes volume; the ERPNext API user should be a dedicated least-privilege user; the
realm import grants the control plane's service account `manage-realm`/`manage-users` (needed for Organizations).

## 10. CRM: one interface, a built-in default, external CRMs as adapters

**Decision.** Sales is the part of the system a customer's own team lives in all day, so the default CRM is built in
(`internal/crm/builtin`, tables in `db/schema_crm.sql`) and everything else talks to the `crm.CRM` interface, never to a
product. A business has **one** CRM of record; external CRMs (HubSpot, Salesforce, Zoho, Odoo, Twenty) are adapters added
one at a time on customer demand, each of which must pass the same contract suite. Nothing about those products is
assumed until an adapter is written against a running instance.

**The interface** (`internal/crm/crm.go`) is tenant-bound (`Provider.For(ctx, tenant)`; no tenant argument to get wrong) and
covers companies, contacts, deals (pipeline, stage, money in minor units, typed custom fields from the blueprint),
stage history and activities (notes, calls, emails, meetings, tasks). Rules every backend follows: portable fields plus a
bounded custom-field map; idempotent creates (replay returns the original, key reuse with different input is an
error); optimistic concurrency via `ifVersion` where the backend can offer it (`Capabilities`); archive instead of
delete; stages are data, not code; oldest-first cursor paging; malformed ids behave like unknown ids.

**Validation lives in one place** (`crm/validate.go`) and every backend calls it, so the same input is accepted or
rejected identically everywhere. Limits count characters, not bytes (a 150-character Hindi name is fine).

**The contract suite** (`internal/crm/crmtest`) is the definition of "correct". It runs against the in-memory
reference (twice: with all capabilities, then with optimistic concurrency and history off, to exercise the path an
adapter for a CRM without versions would take) and against the built-in backend on PostgreSQL. To check the suite can
fail I planted bugs in each backend one at a time (off-by-one cursor, ignored `ifVersion`, un-escaped `%`, ignored
archive, replay ignoring the request hash, a no-op move writing history, aliased maps, unordered pipelines): the suite
caught 14 of the 15 that compiled. The survivor was the row lock on updates, which is why the lock now has its own
tests (below) and a stronger race in the suite.

**What the tests do NOT prove**

* **The row lock only means something on a real multi-connection PostgreSQL.** PGlite (the default test database) has
  one connection, so transactions never overlap; there the lock tests either skip or prove nothing. The built-in
  backend has been run against a real PostgreSQL 16.16 (portable Windows binaries, throwaway cluster): the whole module
  passes there (`VENTURE_TEST_DATABASE_URL=postgres://... go test -p 1 ./...`). On that server a first attempt at proving
  the lock FAILED: with `FOR UPDATE` removed, the original single 8-writer race still passed 6 runs out of 6, because a
  cold connection pool serialises the writers. Two tests now catch it: the suite races 8 writers released at once over 25
  rounds (with the lock removed: 8 of 8 "win" in round 1), and `TestUpdateCannotOverwriteAConcurrentUncommittedChange`
  holds an uncommitted version bump in one transaction and checks a stale writer gets `ErrConflict` instead of silently
  overwriting it (deterministic; fails 3 of 3 with the lock removed). Both pass with the lock. Not yet run against
  PostgreSQL 15 or 17, or with pgvector (`schema_rag.sql` is still untested).
* **Case-insensitive search for non-ASCII text** depends on the database's locale; the suite covers ASCII case folding and
  that Devanagari substrings are found, not (say) an upper-case accented letter against its lower-case form under a C
  locale. Search is `ILIKE '%...%'` with sequential scans: right for thousands of rows per tenant; add `pg_trgm` indexes
  if a tenant outgrows it.
* **Paging under concurrent writes.** The cursor is an identity `seq`; a row whose transaction commits after a later one
  was already served can be skipped by a page iteration in flight. Fine for lists people refresh; do not build an
  exporter on it.
* **`db/apply.sql`** (the incremental psql script) is not run by any test: the portable PostgreSQL package has no `psql`.
  The Go equivalent used for development (`internal/devdb`) is tested, including upgrading a database that predates
  the CRM schema.
* The built-in CRM has no UI yet, no import/export, no duplicate detection (agents are told to search before creating,
  nothing enforces it), no email sync and no custom objects. Archiving a company does not archive its contacts or deals.
* **`crm_ops` has been tested with scripted calls, not with a real model.** What is proven: the 14 agent operations against the
  in-memory backend, and the whole chain (MCP, bus, control plane, PostgreSQL with RLS) including tenant isolation, stale
  versions, strict arguments and audit. What is not: how well an actual agent chooses tools, writes idempotency keys or
  recovers from errors. Watch the first real runs.

**Why provisioning "verifies".** The control plane's seed stage already writes the blueprint's pipelines into
`crm_pipelines` (in blueprint order, through a `position` column). The saga's two CRM steps therefore check that what the
CRM will serve equals the approved blueprint and fail the run otherwise; `RemoveWorkspace` is a no-op by design, because
a rollback step must not be able to delete a business's customers.

**Twenty** is no longer on the critical path. Its containers are still in `docker-compose.yml`; they can be dropped from
the default profile when you decide, and the design in CONNECTORS-DESIGN.md section 1 still applies if a customer wants
it as their CRM.

**Sales screens (Command Center).** People reach the CRM through `CrmService.Execute` (`proto/venture/crm/v1`): the same
`internal/crmops` operations agents use, so validation, versioning and error wording are identical. It differs from the
agent door in who is acting: the caller is a signed-in user who must be a member of the business (a non-member gets
NotFound, as everywhere), reading needs the **viewer** role and any write needs **approver or founder**, and history is
attributed to `user:<sub>` (shown as "You" / "A teammate" / "Agent sales"). Writes are audited under the person.

Behaviour worth knowing: every create form carries an idempotency key issued when the page loads, so a double-click makes
one record, not two (verified in a browser); moves and edits carry the record's version, so editing from a stale page is
refused with "someone else changed this" instead of overwriting; amounts are parsed from text to exact minor units with
no floating point and are rejected rather than rounded; customer-typed text is rendered as text (a note containing
`<script>` shows literally). The pages work without JavaScript (plain form posts) and are single-column on a phone.

Also on the screens: edit a company or contact (versioned, like deals); archive a company, contact or deal from a
two-step "Archive..." section that works without JavaScript (archiving a company does NOT archive its deals, and the
page says how many are left behind); drag a card onto a column to move it (it drives the same selector, which stays for
keyboards); and **CSV export** of companies and of one pipeline's deals (`/b/<id>/crm/export?type=deals|companies`):
exact decimals, UTF-8 with a byte-order mark so Excel reads Devanagari names, quoted per RFC 4180, and customer text
that starts with `= + - @` is prefixed with an apostrophe so a spreadsheet will not run it as a formula. An export over
5,000 rows fails with a message rather than silently returning a partial file. History now names who **created** a deal.

Verified in a browser against the dev stack as each role (`DEV_USER=founder|approver|viewer` picks the seeded person in
dev mode only; the seed adds an approver and a viewer to the demo business): the viewer's pages contain no write forms
or draggable cards and a forced write is refused by the server; the approver can write and is attributed.

**What the screens still do not do:** assign owners (there is no member directory to pick from), import (it needs a
duplicate policy, which is a product decision), show more than the oldest 200 deals on a board (it says so and offers
search), or edit a company's custom fields. A company archived while its deals remain shows those deals as belonging to
"Archived company".


## 11. Meetings and notes connectors, reliability and secrets

A claims-versus-reality review of the platform is in [EVALUATION.md](EVALUATION.md). This section is the design.

**Shape.** Provider-neutral interfaces live in `internal/collab` (a calendar/meeting side and a notes side, plus one set of
validation). Adapters implement them: `connectors/googlecalendar` (events, Google Meet links through
`conferenceDataVersion=1`, free/busy), `connectors/zoom` (server-to-server OAuth), `connectors/notion` (pages, append,
search; text is chunked to Notion's 2,000-character and 100-block limits). A registry (`internal/collabsvc`) is the only
place adapters are built: **connecting verifies the credentials with the provider first and stores them only if they
work**; disconnecting deletes them.

**One HTTP client** (`internal/httpx`) is used by every adapter: token-bucket rate limit, retries only when repeating is
safe (idempotent methods, or a call explicitly marked idempotent; a 429 is always safe to wait out), `Retry-After`
honoured and shared across callers, a circuit breaker, no redirects to another host, bounded response bodies, error
text scrubbed of credentials, and a refusal of any request path containing a `.` or `..` segment (`url.PathEscape` leaves
dots alone, so an agent-supplied id could otherwise redirect a call to another endpoint with the business's credentials).
Adapters also validate agent-supplied ids before they reach a path: Zoom (integer), Notion (UUID), Google Calendar
(`[A-Za-z0-9_-]`). Errors wrap the HTTP status so callers can decide without parsing strings. Incoming
webhooks (none are exposed yet) have a verifier in `internal/webhook`: HMAC, replay window and de-duplication.

**Secrets** (`internal/vault`): each tenant's provider credentials are sealed with AES-256-GCM, bound to the tenant and
provider name as additional data (a sealed secret copied to another tenant will not open), with key versions for
rotation. Credentials are never returned by any RPC; the Settings screen only shows who connected and when.

**Agent tools.** Families `meetings_ops` (`meeting_schedule`, `meeting_cancel`, `calendar_free_busy`) and `notes_ops`
(`notes_search`, `notes_create`, `notes_append`), defined once in `internal/collabops` and used by the bus and the control
plane (agent RPC `CollabOperation`). They are offered when the deployment turns them on (`VENTURE_COLLAB_TOOLS=true`); whether
a given business has connected Google, Zoom or Notion is a per-business fact the bus cannot know, so a call for a provider
that is not connected fails with a plain message telling the agent to ask the founder to connect it.

**Policy.** Anything that would make a provider email other people (a Google event with attendees, or its cancellation)
is not done directly: it queues a `MEETING` approval, and the executor runs it after a human approves, with request id
`approval:<id>` so a retry cannot double-book. Zoom's API sends no invitations, so Zoom meetings are created directly but
with a waiting room and without joining before the host. Zoom and Notion creates go through platform idempotency
(`BeginIdem/CompleteIdem/AbandonIdem`); an outcome of "unknown" is surfaced and never silently repeated.

**Human RPCs.** `ConnectionService` (list providers and what each needs, connect, disconnect; founders only to change)
and `ControlsService` (spend caps, audit verification), reached from the Command Center at `/b/<tenant>/settings`.

**Not done.** The Google consent flow (a founder currently supplies a refresh token); a real-account run of any adapter;
commerce connectors (Shopify, Amazon SP-API, Razorpay, Shiprocket, Odoo).

## 12. Spend caps and the audit chain (schema version 3)

**Spend caps** (`spend_caps`, `internal/store/controls.go`). Per currency: a ceiling per payment, per rolling hour and per
rolling day, any of which may be absent. Only approved `PAYMENT` and `PURCHASE_ORDER` items count toward the hour and day
totals; pending and rejected ones do not. The check runs **inside the `DecideApproval` transaction under a per-tenant
advisory lock**, so two approvers clicking at once cannot both fit under a cap that only one fits under. A refusal
applies to founders too, names the limit that tripped, and leaves the item pending. Limits must be ordered (payment
at most hour at most day) and every change is audited. The audit row for an approval decision and for a cap change is written
in the **same transaction** as the action (before and after values for a cap), so neither can happen without its record; other
human actions are audited just after they take effect, and a failed write is logged with the action and actor, never dropped. Once a business has set a cap in any currency, an approval in a
currency with **no** cap row is refused too (an agent chooses a `PAYMENT`'s currency, so an uncapped currency would otherwise
bypass every ceiling); a business with no caps at all is unrestricted. `CAMPAIGN` and `OTHER` approvals are not capped.

**Approval execution.** An approved item runs its executor once, detached from the request (a closed tab cannot abandon it
half-way). If it never started, failed, or crashed holding the claim (an `EXECUTING` claim older than `store.ExecutionLease`,
5 minutes), approving again with the same payload hash finishes it; executors are idempotent. A live claim is never
taken over. Every request message is capped at 1 MiB before it is parsed.

**Audit chain.** A trigger computes each row's hash from the previous row's hash and length-prefixed canonical fields,
per tenant, under an advisory lock, so concurrent writers cannot fork the chain. `audit_verify()` walks it and returns
the first bad row. This detects an edited or deleted entry in the middle. **It cannot detect deletion of the newest rows**
on its own, because a shorter chain is still a valid chain. Defence: record the head fingerprint (shown on the Settings
screen) somewhere the database administrator cannot write, and compare. Doing that on a schedule is not built.

`db/schema_controls.sql` is schema version 3, `db/schema_rag.sql` version 4, `db/schema_tasks.sql` version 5; `db/apply.sql` guards both and the Go
development applier (`internal/devdb`) upgrades a version-2 database. `apply.sql` itself is not exercised by any test.

## 12a. Agent work queue (schema and fake runner built; the rest is design)

How agents get work, are limited, and are stopped, without depending on Paperclip: [AGENT-TASKS-DESIGN.md](AGENT-TASKS-DESIGN.md).

**Built:** `db/schema_tasks.sql` (schema version 5): tasks, an append-only history written by triggers and the bus, schedules, a
database-enforced state machine, step and attempt limits, a per-business queue cap, and a claim function that is safe with many
dispatchers (a per-business advisory lock, checked on a real PostgreSQL 16). A cancelled task can no longer record tool calls,
whatever the runtime does. `internal/agentrun` has the `Runner` interface, a fake runner and a contract suite.

**Not built:** the Go store, dispatcher, API and screens, token minting per task, the bus code that writes the events, and the
Hermes runner. Hermes has never been run here; its spike is [HERMES-SPIKE.md](HERMES-SPIKE.md). The per-agent tool allow-list
is still **not enforced** until the bus enforcement is built. The cross-business functions rely on owner-only row-level
policies, which the project owner approved as a design on 2026-11-25; the role separation they depend on is not proven by the
test harness (see section 0 of the design).

## 13. Voice core (`internal/voice`)

Scope is the call, not the providers. `Session` takes four interfaces: `Speech` (audio in, transcripts out), `Synth`
(text in, audio out), `Line` (the phone leg: play, stop, hang up) and `Responder` (what to say). Rules: only final,
non-blank transcripts, cut to 1,000 characters, reach the responder; a partial transcript while the agent is talking
stops playback at once and marks the turn interrupted; a new utterance replaces whatever is still being said; time to
first audio is measured per turn (a filler counts as first audio) against an 800 ms budget, with P50/P95 in the summary;
a failing, timed-out or empty responder becomes an apology and a handoff; a goodbye is spoken in full before hanging up;
turn and call-length limits end a stuck call; every call carries `Verified`, false unless the telephony layer proved the
caller (caller ID is spoofable), so the responder must not treat a caller as authenticated by default.

Not built: speech or voice providers, telephony or WebRTC, triggers (such as cart recovery), calling-hours and consent
rules, and the link from the responder to the agent tools. Latency is measured only against fakes; no real figure exists.

## 14. Privacy filter (`internal/dlp`)

Cleans text that comes from outside the business before an agent or a log sees it: redacts cards, Aadhaar, PAN, e-mail, Indian
mobile numbers and credentials; withholds text containing prompt-injection phrases; and quarantines what is left in an
`<untrusted_context>` block with markup escaped. Detection runs after Unicode normalisation, so look-alike letters, invisible
characters and non-Latin digits do not hide data. It is a heuristic first line of defence, **not** a security boundary: the real
controls are that agents have no money-moving tool without a human approval and that tools take no tenant argument. The package is
built and tested and is connected on **one** path: the control plane (`Server.Privacy`, `internal/controlplane/privacy.go`)
screens the `subject` and `body` of CRM activities before an AGENT reads them, while people in the Command Center read the
original and structured fields (e-mail, phone) are left alone. Nothing else calls it yet. See [DLP.md](DLP.md) for what it does and does not catch, how it differs
from the earlier Python version, and which text it should (and should not) be applied to.

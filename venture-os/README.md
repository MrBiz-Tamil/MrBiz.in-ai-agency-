# Venture OS

Multi-tenant, AI-native business operating system. A founder describes a business; the platform designs the
operating model, provisions an isolated workspace (`tenant_id`) across ERP / CRM / Support / Agents, hires an AI
org chart, and runs operations behind human approval gates.

Architecture, tenancy model and network zones: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Build status

| Step | Deliverable | State |
| --- | --- | --- |
| 1 | Monorepo architecture | Done — [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) |
| 2 | Docker Compose infrastructure | Done — written and statically checked; **not yet booted** (see below) |
| 3 | `db/schema.sql` + ports, fakes, domain types | Done — 31 integration assertions pass against Postgres (PGlite); pgvector part untested |
| 5 | Go tenant-scoped MCP server + finance engine | Done: 9 tools, deterministic pricing, approval gating, tested end to end through MCP on real Postgres. Plus 14 `crm_ops` tools when the CRM is real (step 8). support_ops / market_research not built |
| 4 | Go Genesis + Provisioner service (ConnectRPC) | Done against **fake connectors** + real Postgres; see step 7 for the real adapters |
| 5 | Go tenant-scoped MCP server | — |
| 6 | SvelteKit Command Center | Done: portfolio, launch wizard with live blueprint preview, approval inbox, Cmd+K, OIDC BFF. Verified in a browser against the real control plane (simulated connectors); 87 web tests incl. the OIDC flow. **Not** run against real Keycloak or in Docker |
| 7 | Real adapters | Keycloak, ERPNext and Hermes adapters written and tested against in-process fakes of their APIs (Hermes on a real filesystem). **None run against the real product yet.** The LibreDesk adapter is deliberately not written, and Paperclip has been removed from the stack (see docs/ARCHITECTURE.md §9) |
| 8 | Built-in CRM: interface, contract suite, in-memory + PostgreSQL backends | Done: `crm.CRM` interface, a 16-behaviour contract suite, an in-memory reference and the built-in PostgreSQL backend both passing it, provisioning wired (`VENTURE_CONNECTOR_CRM=real`), tested end to end on PGlite and on a real PostgreSQL 16 (whole module passes; the update row lock is covered by a deterministic test that fails without it). `db/apply.sql` and pgvector are still untested. People use it through the **Sales** screens in the Command Center (pipeline board, deals, companies, tasks; verified in a browser as the founder against the dev stack, with demo data seeded); agents through 14 `crm_ops` MCP tools, tested with scripted calls (not a real model). The screens include edit/archive (people only), drag-and-drop and CSV export, and were verified in a browser as founder, approver and viewer. External CRMs (HubSpot, Salesforce, Zoho, Odoo, Twenty) are future adapters behind the same interface. See docs/ARCHITECTURE.md §10 |
| 9 | Meetings and notes connectors | Google Calendar and Meet, Zoom and Notion adapters on a shared reliable HTTP client, per-tenant encrypted credential vault, agent tools `meetings_ops` / `notes_ops`, and a Settings screen to connect them. Anything that emails other people waits for approval. Tested against in-process fakes of each API; **no real Google, Zoom or Notion account has been used**, and the Google consent flow is not built. See docs/ARCHITECTURE.md §11 |
| 10 | Spend caps and tamper-evident audit | Hard per-payment, hourly and daily caps per currency, enforced inside the approval transaction; a hash-chained audit log with a verify button. Both run on a real PostgreSQL 16 as well as PGlite, and the cap race test fails when the lock is removed (checked on that server). The chain cannot detect deletion of the newest rows unless you keep its head fingerprint elsewhere. See §12 |
| 11 | Voice call core | Barge-in, handoff, limits and latency measurement behind interfaces, tested with fakes. **No speech provider, no telephony, no real latency figure.** See §13 |
| 12 | Agent work queue (schema and fake runner) | `db/schema_tasks.sql` (schema version 5): tasks, append-only history, schedules, a state machine and limits enforced by the database, and a claim function for many dispatchers. `internal/agentrun`: the `Runner` interface, a fake runner and a contract suite. Tested on PGlite and real PostgreSQL 16; 22 planted SQL bugs and 8 in the fake were caught by tests (some only after the tests were strengthened). **Not built:** the Go store, dispatcher, API, screens and the Hermes runner; Hermes itself has never been run (see docs/HERMES-SPIKE.md). See docs/AGENT-TASKS-DESIGN.md |
| 13 | Privacy filter | `internal/dlp`: a Go filter (ported from the removed ABOS prototype) that redacts cards, Aadhaar, PAN, e-mail, Indian mobile numbers and credentials, screens for prompt injection and quarantines outsider text. Fixes several gaps in the original (formats, look-alike characters, non-Latin digits, cards hidden among other digits). Tested with fuzzing and planted bugs. **Not yet called by anything**: where to apply it is a decision (docs/DLP.md) |

What is built versus planned, claim by claim: [docs/EVALUATION.md](docs/EVALUATION.md). Commerce connectors (Shopify, Amazon SP-API, Razorpay, Shiprocket, Odoo) are **not built**.

## Try the Command Center (no Docker, no Keycloak, no API key)

```powershell
cd venture-os
scripts\dev.ps1                # PGlite + control plane (fake connectors, demo data) + web on http://127.0.0.1:5190
scripts\dev.ps1 -NoSeed        # start empty and launch your own business with the wizard
scripts\dev.ps1 -AnthropicKey sk-ant-...   # use the real Venture Architect instead of the scripted demo one
```

Sign-in is bypassed and provisioning is simulated; a banner says so. Never expose this to a network.

## Run the tests

```powershell
cd venture-os
go test ./...      # needs node + tools/pgtest deps (cd tools/pgtest; npm ci) for the DB tests; they skip cleanly without them
cd web; npm ci; npm test; npm run check   # Command Center: 87 unit/integration tests + svelte-check
```

Set `VENTURE_TEST_DATABASE_URL` to a throwaway Postgres superuser URL to test against real Postgres instead of PGlite.

## Run the infrastructure

Requires Docker Engine / Docker Desktop with Compose v2.20+ (git build contexts, `include`-free anchors).

```powershell
cd venture-os
scripts\gen-env.ps1                      # writes .env with generated secrets (bash: scripts/gen-env.sh)
# edit .env: set ANTHROPIC_API_KEY (and FOUNDER_EMAIL if you like)
docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d --build
```

The first run builds Hermes from source (several minutes) and ERPNext creates its site (a few more).
Watch progress with `docker compose ps`; every service with a health check should reach `healthy`.

| URL | What |
| --- | --- |
| http://auth.localhost | Keycloak. Admin: `KEYCLOAK_ADMIN` / `KEYCLOAK_ADMIN_PASSWORD` from `.env` |
| http://crm.localhost | Twenty CRM. **Optional**: Venture OS's built-in CRM does not use it |
| http://support.localhost | LibreDesk (login `System` user, password `LIBREDESK_SYSTEM_USER_PASSWORD`) |
| http://erp.localhost | ERPNext (`Administrator` / `ERP_ADMIN_PASSWORD`) |

`*.localhost` resolves to loopback in current Chrome, Edge and Firefox. If it doesn't on your machine, add the
hostnames to your hosts file, or use `curl --resolve`.

After first boot, generate API credentials inside Twenty, LibreDesk and ERPNext and put them in `.env`
(`TWENTY_API_KEY`, `LIBREDESK_API_KEY`/`SECRET`, `ERP_API_KEY`/`SECRET`). They cannot exist
before the products do.

The Venture OS services sit behind compose profiles: `core` (`schema`, `controlplane`; ready), `mcp` (`mcpbus`; step 5)
and `web` (step 6). Start the control plane with `docker compose --profile core up -d --build` after setting
`ANTHROPIC_API_KEY` in `.env`. It runs with `VENTURE_CONNECTORS=fake`: provisioning is simulated in memory.

## Caveats you should know

* **Not run end-to-end.** The authoring machine had no Docker, so the compose file was validated statically
  (YAML parse, anchors/merge keys, shell syntax, JSON), not booted. Expect to fix small things on first boot.
* **Hermes is built from a git tag**, not pulled as an image (no verified upstream image). Pin
  `HERMES_REF` to a commit SHA and push images to your own registry for production.
* **The ABOS prototype is gone.** It was removed from the repository; commit `6198fcd` still holds it.
* **Secrets:** `.env` is gitignored. Regenerating it against existing volumes locks you out of the databases —
  `gen-env` refuses to overwrite unless forced.
* **Hardening still to do for production:** restrict Keycloak `/admin` at the edge, Redis per-service ACL users,
  image digests instead of tags, and backups for `pgdata`, `erp-db-data`, `erp-sites`.

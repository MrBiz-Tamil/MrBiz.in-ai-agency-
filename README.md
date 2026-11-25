# ABOS (Mrbiz.in)

Autonomous Business Operating System. See [PRD.md](PRD.md) and [TECH_SPEC.md](TECH_SPEC.md).

## Status

| Area | State |
| --- | --- |
| Deterministic kernel (policy gate, idempotency, budget breaker) | Implemented, fail-closed, tested against real Redis semantics (miniredis) and real Postgres |
| Ticket lifecycle (policy -> human approval -> execution -> audit) | Implemented (`pkg/tickets`); approvals are compare-and-set, so a ticket executes at most once |
| GraphQL API (queries, mutations, authenticated subscriptions) | Implemented with role checks, tenant isolation, depth/complexity limits |
| Auth | Email + password (bcrypt), hashed server-side sessions, roles (owner/admin/operator/auditor), login rate limiting, single-use websocket tickets |
| Postgres | Migrations 1-4, append-only audit log, **row-level security** (verified under a non-superuser role) |
| MCP gateway + credential vault | Streamable-HTTP MCP client, AES-256-GCM vault bound to the workspace, SSRF protection. Connects to any MCP server; no vendor-specific Shopify/Odoo/Zoho adapters are bundled |
| Python AI service | Policy compiler, DLP shield, LLM router implemented. Goal decomposition / reasoning / heartbeat are canned **dev-only** stubs (a real planner needs an LLM + prompts) |
| Voice | Talker reads the Redis hot cache; the reasoner creates real kernel tickets. The audio pipeline (LiveKit/TTS) is not built |
| Frontend | Login, live execution board, approvals, audit trail, agent management, all on live GraphQL data. Voice widget is a labelled simulation |

## Run locally (Windows / PowerShell)

```powershell
# 1. Database: real Postgres in-process (no Docker). Keep this terminal open.
cd tools\pgtest; npm install
$env:PGTEST_DATA = ".\.data"; node server.mjs 25432

# 2. Seed demo data (workspace, 8 agents, 3 users, sample tickets). The password is yours to pick.
cd ..\..\backend-core
$env:ABOS_ENV = "dev"; $env:ABOS_ADMIN_PASSWORD = "choose-a-password-12+"
$env:DATABASE_URL = "postgres://postgres@127.0.0.1:25432/postgres?sslmode=disable&pool_max_conns=1&default_query_exec_mode=describe_exec"
go run ./cmd/abosadmin seed-dev

# 3. Backend (applies migrations on start in dev; the app role enables row-level security)
$env:ABOS_DB_ROLE = "abos_app"
go run ./cmd/server

# 4. Python AI service (Python 3.12), separate terminal
cd ..\ai-service
py -3.12 -m venv .venv; .venv\Scripts\pip install -r requirements-dev.txt
$env:ABOS_ENV = "dev"; .venv\Scripts\python grpc_server\server.py

# 5. Frontend, separate terminal
cd ..\frontend; npm ci; npm run dev     # http://127.0.0.1:3000  (sign in as operator@demo.test / owner@demo.test / auditor@demo.test)
```

For a real Postgres 16 use its URL instead and drop `pool_max_conns=1`/`default_query_exec_mode`.
`ABOS_ENV` defaults to `production`, which refuses to start without Redis, Postgres and the secrets in
[deploy/env.example](deploy/env.example). Create real users with
`ABOS_ADMIN_PASSWORD=... go run ./cmd/abosadmin create-user <workspace-id> <email> <role>`.

## Tests

```powershell
cd backend-core; go test ./...                 # hermetic; DB tests start PGlite via tools/pgtest (skipped if node/deps are missing)
cd ai-service;   .venv\Scripts\python -m pytest -q
cd frontend;     npm run check; npm run build
# Go <-> Python contract test:
cd backend-core; $env:ABOS_INTEGRATION="1"; $env:ABOS_PYTHON="..\ai-service\.venv\Scripts\python.exe"; go test ./pkg/grpc -run Integration -v
```

## Regenerating gRPC code

`python scripts/generate_contracts.py` rebuilds the Python stubs and, if `buf` and the Go protoc plugins are on
`PATH`, the Go stubs too.

## Known gaps

- Vendor adapters (Shopify, Odoo, Zoho, SAP, Notion) are not bundled; register their MCP servers instead. stdio MCP servers are intentionally unsupported by the hosted backend.
- The webhook queue (`stream:webhooks:incoming`, capped at 100k entries) has no consumer yet: mapping inbound events to tickets needs a routing design.
- Real goal decomposition, agent reasoning and heartbeats (the AI service returns canned data in dev and `UNIMPLEMENTED` in production).
- Voice media pipeline (LiveKit, STT/TTS) and live call telemetry.
- Notion SOP sync for corrections.
- GraphQL subscriptions fan out in-process; running several backend instances needs Redis pub/sub behind `pkg/events`.
- Per-user password reset / invitation flows (users are created with `abosadmin`).

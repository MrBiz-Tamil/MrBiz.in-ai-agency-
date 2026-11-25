# Design: the agent work queue (`agent_tasks`) and the `AgentRunner` interface

Status: **the schema and the fake runner are built and tested; everything else is still design.** The data contract is
`db/schema_tasks.sql` (schema version 5) and the runner contract is `internal/agentrun` with its contract suite
`internal/agentrun/agentruntest`. Where the draft text further down differs from those files, **the files win**; the
differences are listed in the next section. Items marked **Open** still need a decision or a spike against a running Hermes
([HERMES-SPIKE.md](HERMES-SPIKE.md), which has not been run).

## 0. What is built, and where it differs from the draft below

**Built and tested**
* `db/schema_tasks.sql`: `agent_tasks`, `agent_task_events`, `agent_schedules`, the state machine, limits, row-level
  security, and four functions (`agent_task_claim`, `agent_task_expired`, `agent_schedule_due`, `agent_schedule_fire`).
  Wired into `db/apply.sql`, `internal/devdb` and the test database. 11 test functions in `db/tasks_test.go` plus an upgrade test
  in `internal/devdb`. They pass on PGlite and on a real PostgreSQL 16.
* `internal/agentrun`: the `Runner` interface, `Fake` runner, `StartRequest.Validate`, bounds and error codes.
  `agentruntest.Run` is the contract suite (13 behaviours) the fake passes; a real runner must pass it too.
* How it was checked: 22 deliberately planted bugs in the SQL and 8 in the fake and interface were each caught by a test. Four
  were missed at first and led to stronger tests: the step-limit off-by-one (two layers refused it, so the test now expects the
  first layer's message), a late success overwriting a cancel, a schedule replaying missed slots (the "still waiting" rule was
  hiding it), and the claim lock (the first race test never made the dispatchers overlap). A further mutant, removing
  `SKIP LOCKED`, changes only how long a dispatcher waits, not what it gets, so no test can tell it apart. On real PostgreSQL,
  removing the per-business claim lock makes `TestClaimGivesEachTaskToOneDispatcher` fail; on PGlite that test only checks
  bookkeeping because PGlite has one connection.

**Differences from the draft in this document (the files are right)**
1. **Tool calls are written before the tool runs.** Events have kinds `STATE`, `TOOL_CALL`, `TOOL_RESULT`, `NOTE`. A `TOOL_CALL` is
   inserted first (so a crash mid-call still shows `wrote = true`), then a `TOOL_RESULT` records the outcome. A `TOOL_RESULT` is
   accepted even after the task was cancelled, so what happened to an in-flight call is not lost. Events carry an `attempt`.
2. **`STATE` rows can only come from a trigger.** The application role may not insert them (row-level policy), so no one can invent
   a history. The trigger names the person (`app.user_sub`) who made each change.
3. **Cross-business work uses owner-only policies.** `FORCE ROW LEVEL SECURITY` applies to the table owner too, so the claim,
   expiry and due functions are `SECURITY DEFINER` and the tables carry extra policies `TO venture_owner`. This resolves the
   section 9 "Open" item. **Approved by the project owner on 2026-11-25.** It relies on `venture_app` and `venture_owner` being separate
   logins with no membership in each other (true in `infra/postgres/init/01-bootstrap.sh`). The test database emulates
   `venture_app` with `SET ROLE` from a superuser, so that property is **not** proven by the tests.
4. **The database enforces more than the draft promised:** attempt number on every call, "task not running", the step limit
   (twice: on the event and on the task row), counters that only increase, a request that never changes, final states that
   never change, the 200-task queue cap, and the creation rules (active business, active agent, approved skill). Pausing an agent
   stops new work at once.
5. **The claim does not re-check the agent or the business.** Only creation does. The dispatcher must check after a claim and
   fail the task (`agent_unavailable`) if the agent was paused in between.
6. **A schedule's idempotency key truncates the time** (`floor(epoch)`), found when a test showed rounding could name the wrong
   second. A schedule that fell behind resumes from now and never replays missed slots.
7. **A re-run after review keeps `wrote = true`.** A second crash then goes to `NEEDS_REVIEW` again even if the second attempt
   wrote nothing. That is deliberately cautious.
8. **A whitespace-only goal passes the database's length check.** The store must trim and refuse it.
9. **Schema version 5, applied after version 4** (`schema_rag.sql`, pgvector, production only). A development database has
   versions 1, 2, 3 and 5.
10. **Interface additions:** `ErrInvalid`, `ErrConflict`, `StartRequest.Validate`, `Bound`, `MaxSummary`, `MaxDetail`,
    `CodeStepDenied`, and a retried `Start` may carry a fresh token (the same attempt, not a new run).

**Not built:** the Go store (create, list, cancel, resolve, heartbeat), the dispatcher, reaper and scheduler loops, the
`TaskService` RPC and screens, token minting with a `task` claim, the bus enforcement that writes the events, and the Hermes
runner. **The interface may change after the Hermes spike** (questions 4 and 5).

## 1. Why this exists

Venture OS decided not to depend on Paperclip for orchestration (see the recommendation in the conversation that led to
this document, summarised in section 12). Something still has to answer: *what work is the agent doing, who asked, did it
finish, what did it cost, and can I stop it?* That is the work queue. Hermes stays a replaceable worker behind an
interface; approvals, caps, audit and tenant isolation stay in Venture OS.

**Goals**
* One place to see and control everything agents do, per business.
* An agent can only do what its task allows, and a human can stop it instantly.
* A crash never causes a silent duplicate of a side effect (same rule as the connectors).
* Hermes can be replaced by another runtime without touching approvals, CRM or audit.

**Non-goals (v1):** agents delegating to other agents, multi-step human conversations inside one run, streaming the
agent's reasoning to the UI, an org-chart board view, per-task billing.

## 2. Names

`ports.AgentRuntime` already exists: it provisions a Hermes *profile* and skills (`EnsureProfile`, `InstallSkills`,
`RemoveProfile`). The new interface runs *work*, so it is called **`AgentRunner`** and lives in its own package,
`internal/agentrun`. A Hermes adapter would implement both.

## 3. How a task flows

```
create (founder / schedule / webhook / approval decision)
   └─► QUEUED ──claim──► RUNNING ──┬─► DONE
                            │      ├─► FAILED
                            │      ├─► CANCELLED   (by a human, any time before a terminal state)
                            │      └─► NEEDS_REVIEW (outcome unknown after a crash or timeout)
                            └─ lease expires, nothing was written ─► QUEUED (retry, up to max_attempts)
```

**Design decision: a run never waits for a human.** When the agent calls a tool that needs approval (`request_human_approval`,
`create_quote`, a meeting with attendees, ...) the bus already returns "queued for approval" at once. The agent should
finish its run. When a person later approves or rejects, the control plane creates a **follow-up task**
(`source = 'APPROVAL'`, idempotency key `approval:<id>:<decision>`) that tells the agent the outcome. This avoids runs
held open for hours, avoids leases that must outlive a human, and makes every run short and replaceable. The cost is
that the agent gets context only from the goal text and the linked approval, not from a live conversation.

## 4. Data model (draft text; `db/schema_tasks.sql` is the source of truth, see section 0)

```sql
CREATE TABLE agent_tasks (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL REFERENCES organizations(id),
  agent_key        text NOT NULL,
  goal             text NOT NULL CHECK (char_length(goal) BETWEEN 1 AND 4000),
  source           text NOT NULL CHECK (source IN ('FOUNDER','SCHEDULE','WEBHOOK','APPROVAL')),
  source_ref       text,                         -- schedule id, webhook event id, approval id
  untrusted        boolean NOT NULL DEFAULT false, -- goal contains text typed by an outsider (see 7)
  status           text NOT NULL DEFAULT 'QUEUED'
                   CHECK (status IN ('QUEUED','RUNNING','DONE','FAILED','CANCELLED','NEEDS_REVIEW')),
  priority         smallint NOT NULL DEFAULT 5 CHECK (priority BETWEEN 1 AND 9),   -- 1 = most urgent
  not_before       timestamptz NOT NULL DEFAULT now(),
  -- limits, fixed at creation and copied into the run
  max_steps        int  NOT NULL DEFAULT 25  CHECK (max_steps  BETWEEN 1 AND 200),
  max_seconds      int  NOT NULL DEFAULT 600 CHECK (max_seconds BETWEEN 10 AND 3600),
  max_attempts     smallint NOT NULL DEFAULT 2 CHECK (max_attempts BETWEEN 1 AND 5),
  -- execution state
  attempt          smallint NOT NULL DEFAULT 0,
  lease_owner      text,
  lease_expires_at timestamptz,
  run_ref          text,                          -- the runner's id for the current attempt
  steps_used       int NOT NULL DEFAULT 0,
  wrote            boolean NOT NULL DEFAULT false, -- a non-read-only tool call was recorded for this task
  cost_minor       bigint NOT NULL DEFAULT 0 CHECK (cost_minor >= 0),
  cost_currency    char(3) NOT NULL DEFAULT 'USD' CHECK (cost_currency ~ '^[A-Z]{3}$'),
  cost_reported    boolean NOT NULL DEFAULT false, -- false = the runner gave no usage figures
  -- outcome
  result_summary   text CHECK (char_length(result_summary) <= 4000),
  error_code       text,
  error_detail     text CHECK (char_length(error_detail) <= 2000),
  created_by       text NOT NULL,                  -- user:<sub> | schedule:<id> | webhook:<provider> | approval:<id>
  idempotency_key  text NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  started_at       timestamptz,
  finished_at      timestamptz,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, idempotency_key),
  FOREIGN KEY (tenant_id, agent_key) REFERENCES agent_org_configs (tenant_id, agent_key),
  CHECK ((status = 'RUNNING') = (lease_owner IS NOT NULL)),
  CHECK (status NOT IN ('DONE','FAILED','CANCELLED') OR finished_at IS NOT NULL)
);
CREATE INDEX agent_tasks_queue ON agent_tasks (priority, not_before, created_at) WHERE status = 'QUEUED';
CREATE INDEX agent_tasks_lease ON agent_tasks (lease_expires_at) WHERE status = 'RUNNING';
CREATE INDEX agent_tasks_tenant ON agent_tasks (tenant_id, created_at DESC);

-- What the agent actually did, written by the MCP bus (not by the agent or the runner). Append-only.
CREATE TABLE agent_task_events (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id    uuid NOT NULL REFERENCES organizations(id),
  task_id      uuid NOT NULL,
  seq          int  NOT NULL,                      -- 1,2,3... per task, assigned under the task's row lock
  kind         text NOT NULL CHECK (kind IN ('STATE','TOOL_CALL','NOTE')),
  tool         text,
  read_only    boolean,
  ok           boolean,
  approval_id  uuid,                               -- set when the call queued an approval
  detail       jsonb NOT NULL DEFAULT '{}',        -- bounded and redacted by the writer, never raw credentials
  created_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (task_id, seq),
  FOREIGN KEY (tenant_id, task_id) REFERENCES agent_tasks (tenant_id, id),
  FOREIGN KEY (tenant_id, approval_id) REFERENCES approval_queue (tenant_id, id)
);

-- Recurring work. Each firing creates one task with key 'sched:<id>:<fire time>', so a double fire is one task.
CREATE TABLE agent_schedules (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    uuid NOT NULL REFERENCES organizations(id),
  agent_key    text NOT NULL,
  goal         text NOT NULL CHECK (char_length(goal) BETWEEN 1 AND 4000),
  every_minutes int NOT NULL CHECK (every_minutes BETWEEN 15 AND 10080),   -- cron syntax deliberately not offered in v1
  enabled      boolean NOT NULL DEFAULT true,
  next_run_at  timestamptz NOT NULL,
  created_by   text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, agent_key) REFERENCES agent_org_configs (tenant_id, agent_key)
);
```

Conventions followed from the existing schema: composite `(tenant_id, id)` keys, row-level security with the same four
policies as the other tenant tables, `venture_app` is `NOBYPASSRLS`, `touch_updated_at()` trigger, and the audit trigger
on state changes. `agent_task_events` gets **no UPDATE or DELETE grant**.

**State-transition guard (trigger, like `approval_queue`).** Allowed moves only:

| From | To |
| --- | --- |
| QUEUED | RUNNING, CANCELLED |
| RUNNING | DONE, FAILED, CANCELLED, NEEDS_REVIEW, QUEUED (only if `wrote = false` and `attempt < max_attempts`) |
| NEEDS_REVIEW | QUEUED (a human chose "run again"), DONE, FAILED, CANCELLED |
| DONE, FAILED, CANCELLED | nothing |

`goal`, `agent_key`, the limits and `idempotency_key` cannot change after creation.

**Creation rules (in the store, not only the UI):** the agent must exist with `status = 'ACTIVE'` and
`skill_status = 'APPROVED'`; the tenant must be ACTIVE; the agent's budget must not be exhausted (section 8).

## 5. The runner interface (draft text; `internal/agentrun` is the source of truth. It is named `Runner` there)

```go
package agentrun

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/ports"
)

// Runner starts and watches one unit of agent work. It knows nothing about approvals, caps or the database:
// everything the agent may do goes through the MCP bus using the token in StartRequest.
type Runner interface {
	// Start begins the run. It MUST be idempotent on Ref: calling it again with the same Ref returns the run
	// that already exists instead of starting a second one.
	Start(ctx context.Context, req StartRequest) error
	// Status reports where the run is. An unknown Ref returns ErrNotFound (it never started).
	Status(ctx context.Context, ref Ref) (Status, error)
	// Cancel stops the run. Idempotent; cancelling a finished or unknown run is success.
	Cancel(ctx context.Context, ref Ref) error
}

// Ref names one attempt. The dispatcher derives it from the task and attempt number, so after a crash or a timed-out
// Start it can ask "did that start?" without having received anything back.
type Ref string

func RefFor(task uuid.UUID, attempt int) Ref // "<task id>.<attempt>"

type StartRequest struct {
	Ref       Ref
	Tenant    ports.TenantInfo
	AgentKey  string
	Profile   ports.Ref // the Hermes profile created at provisioning
	Goal      string
	Untrusted bool      // the goal includes outsider-typed text: the runner must present it as data (section 7)
	Token     string    // short-lived, task-scoped MCP token (section 6)
	Limits    Limits
}

type Limits struct {
	MaxSteps int
	Deadline time.Time // absolute; the runner stops the run itself at this time
}

type State int

const (
	Running State = iota + 1
	Succeeded
	Failed
	Cancelled
)

type Status struct {
	State   State
	Summary string // the agent's own closing summary, bounded; shown to humans, never acted on
	Usage   Usage
	Err     *RunError // set when State == Failed
}

type Usage struct {
	Reported     bool // false when the runtime gives no figures: do not treat zero as free
	InputTokens  int
	OutputTokens int
	CostMinor    int64
	Currency     string
}

type RunError struct {
	Code   string // stable, e.g. "deadline", "step_limit", "model_error", "runtime_unavailable"
	Detail string // bounded, scrubbed
}

var (
	ErrNotFound  = errors.New("agentrun: no such run")
	ErrTransient = ports.ErrTransient // retry the call; the run's fate is not known
)
```

**Why these shapes**
* **`Ref` derived by the caller** answers the hardest crash question: the dispatcher times out on `Start`, so did the
  agent start? It calls `Status(Ref)`: `ErrNotFound` means no, safe to retry; anything else means yes. **Open:** this needs
  Hermes to accept a caller-chosen run id or an idempotency key. If it does not, the adapter must keep its own mapping,
  or the dispatcher falls back to the `wrote` flag (section 9).
* **Polling, not callbacks**, for v1: fewer moving parts and no inbound endpoint on the control plane. Poll every 2 s with
  backoff.
* **The runner returns a summary, not results the platform acts on.** Any effect on a business comes only from tool
  calls the bus saw. A runner that lies about success cannot create a CRM record or a payment.
* **`Usage.Reported`** exists because it is unverified that Hermes reports usage. Zero must not look like "free".

A **fake runner** (scripted tool-call sequences, controllable failures, hangs and crashes) ships with the interface, and
a **contract suite** that every real runner must pass: idempotent `Start`, `Status` of an unknown ref, idempotent
`Cancel`, deadline enforced, limits enforced, no goroutine or process left after `Cancel`.

## 6. Authority: the task-scoped token and what the bus enforces

Today the provisioner mints one token per tenant profile, so all of a tenant's agents share it and the per-agent tool
allow-list (`agent_org_configs.tools_allowed`) is not enforced (a known gap in `ARCHITECTURE.md` section 7). This design
fixes that by minting **one token per task attempt**:

`mcptoken.Mint(tenant, agent, ttl)` already carries the agent. Add a `task` claim and set `ttl = max_seconds + 60 s`.

The bus, on **every** call with such a token:

1. Looks up the task. If it is not `RUNNING`, or the token's attempt is not the current one, the call fails. **This is the
   kill switch**: cancelling a task makes the agent's token useless immediately, even if Hermes ignores the cancel.
2. Rejects any tool not in the agent's `tools_allowed`.
3. Rejects the call when `steps_used >= max_steps`, otherwise increments it.
4. Writes a `TOOL_CALL` event (tool, read-only or not, ok, approval id) and, for a non-read-only call, sets `wrote = true`,
   in the same transaction as the tool's own effect where the tool is a database write.
5. Stamps any approval it creates with the task id (the approval detail page can then say "requested by task ...").

Tokens for runs started outside the queue (the existing profile-level token) keep working during migration; a flag then
turns them off.

## 7. Untrusted input

A founder's typed goal is trusted. A goal built from a customer's email, a webhook body or a CRM note is not. For
`untrusted = true` tasks:
* the dispatcher wraps the outsider's text in a clearly delimited block and the Hermes skill text tells the agent it is
  **data, not instructions** (the same convention the `crm_ops` tool descriptions use);
* the token's allowed tool set is **read-only tools plus approval-gated ones**: no direct writes (`crm_create_*`,
  `notes_create`, ...) and no `meeting_schedule` without attendees check;
* anything it wants to send out goes to the approval inbox as usual.

This limits prompt-injection damage to "a human sees a strange approval request". It does not make injection impossible.

## 8. Limits and cost

| Limit | Where enforced |
| --- | --- |
| Steps per task | The bus (section 6), not the runner. |
| Time per task | The runner (`Deadline`) **and** the lease/reaper, **and** the token's expiry. Three independent stops. |
| Attempts | The reaper and `RUNNING -> QUEUED` guard. |
| Concurrency | Per tenant (default 2) and per agent (default 1), checked at claim time. |
| Queue depth | At most 200 `QUEUED` tasks per tenant; creating more is refused with a plain message. |
| Spend on the model | `agent_org_configs.monthly_budget_minor` is **not enforced today**. With `Usage.Reported` true, the dispatcher refuses to start a task when this month's `cost_minor` for the agent has reached the budget. With it false, only steps and time bound the cost, and the UI says "cost not measured". **Open:** whether Hermes can report usage. |

This budget is about **model cost** and is separate from the spend caps on business money (payments, purchase orders),
which are enforced at approval time and cannot be raised by an agent.

## 9. The dispatcher

A loop in the control plane (one goroutine per replica; replicas are safe).

**Claim.** Claiming spans tenants, but row-level security filters by `app_tenant_id()`. **Open:** use a
`SECURITY DEFINER` function `agent_task_claim(owner text, lease interval, n int)`, owned by `venture_owner`, that returns
only `(tenant_id, task_id)` after applying the concurrency limits with `FOR UPDATE SKIP LOCKED`. All further work on the
task happens inside a normal tenant-scoped transaction. The function can see queue metadata and nothing else.

**Run.**
```
claim → (tenant tx) set RUNNING, attempt++, lease_owner, lease_expires_at = now + max_seconds + 30s, run_ref = RefFor(id, attempt)
      → mint task token
      → Runner.Start(ref)            // on ErrTransient: Status(ref); ErrNotFound -> retry Start, else carry on
      → every 2 s: Runner.Status(ref); extend the lease while Running
      → Succeeded   → DONE (+ summary, usage)      → create nothing else; approvals the agent queued stay in the inbox
        Failed      → FAILED (+ error code)         → attempts left and wrote = false -> QUEUED with backoff
        Cancelled   → CANCELLED
```

**Reaper** (every 30 s): for `RUNNING` rows whose lease expired:
* ask the runner `Status(ref)`; if it is still running, **cancel it**, then continue;
* if `wrote = false` and attempts remain: back to `QUEUED` (nothing happened, so repeating is safe);
* otherwise → **`NEEDS_REVIEW`**. The agent may have written something. A human sees the events list ("it created deal X
  and queued approval Y before stopping") and chooses *run again*, *mark done* or *mark failed*. **It is never repeated
  automatically**, the same rule the Zoom and Notion creates follow.

A task whose runner reports success after the lease expired is still ignored if the task has already moved on: state
changes are compare-and-set on `(id, attempt, status)`.

**Scheduler** (every 30 s): for each enabled schedule with `next_run_at <= now`, create a task with key
`sched:<id>:<next_run_at>` and advance `next_run_at` in the same transaction. A schedule that falls behind runs **once**
(no catch-up storm), and a schedule whose last task is still not finished is skipped.

## 10. API and screens

`TaskService` (ConnectRPC, `proto/venture/task/v1`), human callers only, tenant membership required (non-member gets
NotFound, as everywhere):

| RPC | Role | Notes |
| --- | --- | --- |
| `CreateTask` | approver, founder | Takes an idempotency key issued by the page (double-click = one task). |
| `ListTasks`, `GetTask` (with events) | viewer | Filter by status and agent. |
| `CancelTask` | approver, founder | Works from QUEUED and RUNNING; sets the state, which also kills the token. |
| `ResolveTask` | founder | Only from NEEDS_REVIEW: run again / mark done / mark failed. |
| `Create/List/Update/DeleteSchedule` | founder | Limited to agents the founder can see. |

Agents have **no** RPC to create tasks in v1 (no agent-to-agent delegation). The bus writes events through the store
directly.

Command Center: `/b/[tenant]/tasks` (list with status chips, "Give an agent a goal" form, "Needs review" section first),
a task page (goal, who asked, timeline of tool calls with approval links, summary, cost or "cost not measured", Cancel),
and a schedules section. Same conventions as the CRM screens: works without JavaScript, single column on a phone, model
output rendered as text.

## 11. Audit, privacy and failure behaviour

* Every state change and every `CreateTask`, `CancelTask`, `ResolveTask` is audited under the person or `agent:<key>`
  (the audit chain from schema version 3 applies).
* `goal` and `result_summary` may contain business or customer data: they are tenant-scoped, covered by RLS, shown only to
  members, and never sent to logs. Event `detail` is size-capped and redacted.
* Hermes down: `Start` fails with `ErrTransient`; the task stays QUEUED with backoff (1 min, 5 min, 15 min) and shows
  "agent runtime unavailable". After `max_attempts` it is FAILED with code `runtime_unavailable`.
* Control plane down: leases expire and the reaper applies section 9 on restart.
* Retention: terminal tasks and events are kept 180 days by default (**Open**: needs a policy and a purge that respects
  the audit chain, which must not lose rows from the middle).

## 12. Decisions behind this design (and what would change them)

| Decision | Why | Revisit if |
| --- | --- | --- |
| Own work queue, not Paperclip | One rulebook; Paperclip needs a bridge and a service credential that is not documented | Customers ask for its board |
| Runs never wait for humans | Short, replaceable runs; no long leases | Agents need back-and-forth with a person inside one run |
| Poll, not callbacks | No inbound endpoint, simpler to test | Poll load or latency matters |
| `NEEDS_REVIEW` instead of auto-retry after a write | Consistent with connector idempotency rules; avoids duplicate side effects | The `wrote` signal proves reliable and tasks are naturally idempotent |
| Task-scoped tokens enforced at the bus | Closes the allow-list gap and gives an instant kill switch | Hermes cannot send a per-run token |
| Interval schedules only | Cron needs time-zone and DST decisions | Customers need "9 am weekdays in IST" |

## 13. Test plan

1. **State machine:** a table test of every allowed and forbidden transition against PostgreSQL, plus mutation testing of
   the guard.
2. **Claim race on a real multi-connection PostgreSQL** (PGlite has one connection and cannot prove this): 8 dispatchers,
   one task, exactly one claim; per-tenant concurrency limit holds under contention.
3. **Lease and reaper with a fake clock:** expiry with `wrote = false` requeues; with `wrote = true` goes to NEEDS_REVIEW;
   a late success after requeue is ignored.
4. **Crash windows:** kill the dispatcher after `Start` but before the status write, and after a tool write but before
   completion; assert no duplicate effect and the right final state.
5. **Bus enforcement:** disallowed tool, step limit, cancelled task, expired token, a token from attempt 1 after attempt 2
   began, a token for another tenant's task.
6. **Isolation:** tenant B cannot list, read, cancel or resolve tenant A's tasks or events.
7. **Untrusted tasks:** a write tool is refused for `untrusted = true`.
8. **Runner contract suite** run against the fake now and against Hermes once a live instance exists.
9. **Schedules:** double fire creates one task; falling behind runs once.
10. **UI:** double-click creates one task; viewer sees no write controls; a forced write is refused by the server.

## 14. Build order and what must be proven first

0. **Spike (before any code), against a running Hermes:** can it be started with a caller-chosen run id or idempotency key;
   how does it receive a per-run token and a goal; does it report usage; how does it stop a run; can two runs for two
   tenants be isolated (memory, files). **The answers may change section 5.**
1. `schema_tasks.sql`, the store, state-guard tests, RLS tests.
2. `internal/agentrun` with the fake runner and the contract suite.
3. Bus enforcement (token claim, allow-list, steps, events, kill switch). This is useful even before the dispatcher.
4. Dispatcher, reaper, scheduler, with the fake runner and a fake clock.
5. `TaskService` and the screens.
6. The Hermes runner, behind a flag, then a supervised run with real transcripts.
7. Webhook- and approval-created tasks (these depend on LibreDesk and on the approval executor).

## 15. What this does not solve

* The shared-Hermes isolation question (memory and files inside one Hermes process). Tokens limit what an agent can do to
  your data; they do not limit what it can read from another business's profile inside Hermes. Decide between a shared
  runtime for a pilot and one container per business for customers.
* Model quality: a well-governed agent can still choose badly. The approval inbox is the safeguard, and you must read
  real transcripts early.
* Anything in section 14 step 0. Until Hermes has been run for real, sections 5 and 9 are a plan, not a fact.

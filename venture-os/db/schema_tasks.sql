-- Venture OS agent work queue (schema version 5). Applied as venture_owner after schema.sql, schema_crm.sql and
-- schema_controls.sql (schema_rag.sql, version 4, is independent and production-only).
--
-- Design: docs/AGENT-TASKS-DESIGN.md. In short:
--   * agent_tasks: what agents are asked to do, who asked, limits, state, outcome. A guard trigger enforces the state
--     machine, immutability of what was asked, monotonic counters and the creation rules.
--   * agent_task_events: append-only record of what happened. STATE rows are written by a trigger (nobody can forge one);
--     TOOL_CALL / TOOL_RESULT / NOTE rows are written by the MCP bus. The database itself refuses a tool call when the
--     task is not RUNNING, when the attempt is stale or when the step limit is reached: a cancelled task stops being able
--     to act even if the runtime ignores the cancel.
--   * agent_schedules: recurring work; each firing creates one task keyed by schedule and time, so a double fire is one task.
--   * Four functions for work that spans tenants (claim, list expired, list due) run as SECURITY DEFINER under narrow
--     owner-only policies, and return queue metadata only. Everything else is tenant-scoped like every other table.
--
-- NOT built here: the Go store, the dispatcher, the TaskService RPC and screens. This file is the data contract.

BEGIN;

-- ───────────────────────────── agent_tasks ─────────────────────────────
CREATE TABLE agent_tasks (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL REFERENCES organizations(id),
  agent_key        text NOT NULL,
  goal             text NOT NULL CHECK (char_length(goal) BETWEEN 1 AND 4000),
  source           text NOT NULL CHECK (source IN ('FOUNDER','SCHEDULE','WEBHOOK','APPROVAL')),
  source_ref       text CHECK (char_length(source_ref) <= 200),
  untrusted        boolean NOT NULL DEFAULT false,          -- the goal embeds text typed by an outsider
  status           text NOT NULL DEFAULT 'QUEUED'
                   CHECK (status IN ('QUEUED','RUNNING','DONE','FAILED','CANCELLED','NEEDS_REVIEW')),
  priority         smallint NOT NULL DEFAULT 5 CHECK (priority BETWEEN 1 AND 9),   -- 1 = most urgent
  not_before       timestamptz NOT NULL DEFAULT now(),
  max_steps        int      NOT NULL DEFAULT 25  CHECK (max_steps    BETWEEN 1 AND 200),
  max_seconds      int      NOT NULL DEFAULT 600 CHECK (max_seconds  BETWEEN 10 AND 3600),
  max_attempts     smallint NOT NULL DEFAULT 2   CHECK (max_attempts BETWEEN 1 AND 5),
  attempt          smallint NOT NULL DEFAULT 0   CHECK (attempt >= 0),
  lease_owner      text,
  lease_expires_at timestamptz,
  run_ref          text,                                     -- "<task id>.<attempt>": agentrun.RefFor
  steps_used       int      NOT NULL DEFAULT 0 CHECK (steps_used >= 0),
  wrote            boolean  NOT NULL DEFAULT false,          -- a non-read-only tool call was started for this task
  cost_minor       bigint   NOT NULL DEFAULT 0 CHECK (cost_minor >= 0),
  cost_currency    char(3)  NOT NULL DEFAULT 'USD' CHECK (cost_currency ~ '^[A-Z]{3}$'),
  cost_reported    boolean  NOT NULL DEFAULT false,          -- false: the runtime gave no usage figures; zero is not "free"
  result_summary   text CHECK (char_length(result_summary) <= 4000),
  error_code       text CHECK (char_length(error_code) <= 100),
  error_detail     text CHECK (char_length(error_detail) <= 2000),
  created_by       text NOT NULL CHECK (char_length(created_by) BETWEEN 3 AND 200),
  idempotency_key  text NOT NULL CHECK (char_length(idempotency_key) BETWEEN 1 AND 200),
  created_at       timestamptz NOT NULL DEFAULT now(),
  started_at       timestamptz,
  finished_at      timestamptz,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, idempotency_key),
  FOREIGN KEY (tenant_id, agent_key) REFERENCES agent_org_configs (tenant_id, agent_key),
  CHECK ((status = 'RUNNING') = (lease_owner IS NOT NULL)),
  CHECK ((status = 'RUNNING') = (lease_expires_at IS NOT NULL)),
  CHECK (status NOT IN ('DONE','FAILED','CANCELLED') OR finished_at IS NOT NULL),
  CHECK (status <> 'FAILED' OR error_code IS NOT NULL),
  -- who asked must match how the task came to exist
  CHECK ((source = 'FOUNDER'  AND created_by LIKE 'user:%')
      OR (source = 'SCHEDULE' AND created_by LIKE 'schedule:%')
      OR (source = 'WEBHOOK'  AND created_by LIKE 'webhook:%')
      OR (source = 'APPROVAL' AND created_by LIKE 'approval:%'))
);
CREATE INDEX agent_tasks_queue  ON agent_tasks (priority, not_before, created_at) WHERE status = 'QUEUED';
CREATE INDEX agent_tasks_lease  ON agent_tasks (lease_expires_at) WHERE status = 'RUNNING';
CREATE INDEX agent_tasks_tenant ON agent_tasks (tenant_id, created_at DESC);
CREATE INDEX agent_tasks_source ON agent_tasks (tenant_id, source, source_ref) WHERE source_ref IS NOT NULL;

CREATE FUNCTION agent_tasks_guard() RETURNS trigger
LANGUAGE plpgsql AS
$$
DECLARE
  org_status   text;
  a_status     text;
  a_skill      text;
  queued       int;
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'QUEUED' OR NEW.attempt <> 0 OR NEW.lease_owner IS NOT NULL OR NEW.wrote OR NEW.steps_used <> 0
       OR NEW.cost_minor <> 0 OR NEW.started_at IS NOT NULL OR NEW.finished_at IS NOT NULL OR NEW.result_summary IS NOT NULL
       OR NEW.error_code IS NOT NULL THEN
      RAISE EXCEPTION 'a task must be created QUEUED and untouched' USING ERRCODE = 'check_violation';
    END IF;

    SELECT status INTO org_status FROM organizations WHERE id = NEW.tenant_id;
    IF org_status IS DISTINCT FROM 'ACTIVE' THEN
      RAISE EXCEPTION 'the business is not active (%): no work can be queued', coalesce(org_status, 'unknown')
        USING ERRCODE = 'check_violation';
    END IF;
    SELECT status, skill_status INTO a_status, a_skill FROM agent_org_configs
      WHERE tenant_id = NEW.tenant_id AND agent_key = NEW.agent_key;
    IF NOT FOUND THEN
      RAISE EXCEPTION 'unknown agent %', NEW.agent_key USING ERRCODE = 'check_violation';
    END IF;
    IF a_status <> 'ACTIVE' THEN
      RAISE EXCEPTION 'agent % is not active (%)', NEW.agent_key, a_status USING ERRCODE = 'check_violation';
    END IF;
    IF a_skill <> 'APPROVED' THEN
      RAISE EXCEPTION 'agent % has no approved skill (%)', NEW.agent_key, a_skill USING ERRCODE = 'check_violation';
    END IF;

    PERFORM pg_advisory_xact_lock(hashtextextended('agent_tasks:' || NEW.tenant_id::text, 0));
    SELECT count(*) INTO queued FROM agent_tasks WHERE tenant_id = NEW.tenant_id AND status = 'QUEUED';
    IF queued >= 200 THEN
      RAISE EXCEPTION 'the queue is full (200 waiting tasks): wait for some to finish or cancel some'
        USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
  END IF;

  -- What was asked for never changes after the fact.
  IF (NEW.agent_key, NEW.goal, NEW.source, NEW.source_ref, NEW.untrusted, NEW.priority, NEW.max_steps, NEW.max_seconds,
      NEW.max_attempts, NEW.created_by, NEW.idempotency_key, NEW.created_at)
     IS DISTINCT FROM
     (OLD.agent_key, OLD.goal, OLD.source, OLD.source_ref, OLD.untrusted, OLD.priority, OLD.max_steps, OLD.max_seconds,
      OLD.max_attempts, OLD.created_by, OLD.idempotency_key, OLD.created_at) THEN
    RAISE EXCEPTION 'task % request is immutable', OLD.id USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF OLD.status IN ('DONE','FAILED','CANCELLED') THEN
    RAISE EXCEPTION 'task % is final (%)', OLD.id, OLD.status USING ERRCODE = 'integrity_constraint_violation';
  END IF;

  -- Counters only move forward: nothing can un-spend a step or un-write.
  IF NEW.steps_used < OLD.steps_used OR NEW.cost_minor < OLD.cost_minor OR (OLD.wrote AND NOT NEW.wrote) THEN
    RAISE EXCEPTION 'task % counters only increase', OLD.id USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF NEW.steps_used > NEW.max_steps THEN
    RAISE EXCEPTION 'task % exceeded its step limit', OLD.id USING ERRCODE = 'integrity_constraint_violation';
  END IF;

  IF NEW.status <> OLD.status THEN
    IF NOT (
         (OLD.status = 'QUEUED'       AND NEW.status IN ('RUNNING','CANCELLED')) OR
         (OLD.status = 'RUNNING'      AND NEW.status IN ('DONE','FAILED','CANCELLED','NEEDS_REVIEW','QUEUED')) OR
         (OLD.status = 'NEEDS_REVIEW' AND NEW.status IN ('QUEUED','DONE','FAILED','CANCELLED'))) THEN
      RAISE EXCEPTION 'illegal task transition % -> %', OLD.status, NEW.status
        USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'RUNNING' AND NEW.status = 'QUEUED' THEN
      -- Repeating is only safe when nothing was written, and only while attempts remain.
      IF OLD.wrote THEN
        RAISE EXCEPTION 'task % may have written something: it needs review, not a retry', OLD.id
          USING ERRCODE = 'integrity_constraint_violation';
      END IF;
      IF OLD.attempt >= OLD.max_attempts THEN
        RAISE EXCEPTION 'task % has used all % attempts', OLD.id, OLD.max_attempts
          USING ERRCODE = 'integrity_constraint_violation';
      END IF;
    END IF;
    IF NEW.status = 'RUNNING' THEN
      IF NEW.attempt <> OLD.attempt + 1 THEN
        RAISE EXCEPTION 'starting a task must be attempt %', OLD.attempt + 1 USING ERRCODE = 'integrity_constraint_violation';
      END IF;
      NEW.started_at := coalesce(OLD.started_at, now());
    ELSIF NEW.attempt <> OLD.attempt THEN
      RAISE EXCEPTION 'attempt changes only when a run starts' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.status IN ('DONE','FAILED','CANCELLED') THEN
      NEW.finished_at := coalesce(NEW.finished_at, now());
    END IF;
  ELSIF NEW.attempt <> OLD.attempt THEN
    RAISE EXCEPTION 'attempt changes only when a run starts' USING ERRCODE = 'integrity_constraint_violation';
  END IF;

  -- A lease exists only while RUNNING.
  IF NEW.status <> 'RUNNING' THEN
    NEW.lease_owner := NULL;
    NEW.lease_expires_at := NULL;
  END IF;
  NEW.updated_at := now();
  RETURN NEW;
END
$$;
CREATE TRIGGER agent_tasks_guard BEFORE INSERT OR UPDATE ON agent_tasks
  FOR EACH ROW EXECUTE FUNCTION agent_tasks_guard();

-- ───────────────────────────── agent_task_events ─────────────────────────────
CREATE TABLE agent_task_events (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id    uuid NOT NULL REFERENCES organizations(id),
  task_id      uuid NOT NULL,
  attempt      smallint NOT NULL,
  seq          int NOT NULL,                       -- 1, 2, 3 ... per task; assigned by trigger under the task's row lock
  kind         text NOT NULL CHECK (kind IN ('STATE','TOOL_CALL','TOOL_RESULT','NOTE')),
  tool         text CHECK (char_length(tool) <= 100),
  read_only    boolean,
  ok           boolean,
  approval_id  uuid,                               -- set when the call queued an approval
  detail       jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(detail) = 'object' AND octet_length(detail::text) <= 8192),
  created_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (task_id, seq),
  FOREIGN KEY (tenant_id, task_id) REFERENCES agent_tasks (tenant_id, id),
  FOREIGN KEY (tenant_id, approval_id) REFERENCES approval_queue (tenant_id, id),
  CHECK (kind <> 'TOOL_CALL'   OR (tool IS NOT NULL AND read_only IS NOT NULL)),
  CHECK (kind <> 'TOOL_RESULT' OR (tool IS NOT NULL AND ok IS NOT NULL))
);
CREATE INDEX agent_task_events_task ON agent_task_events (tenant_id, task_id, seq);

-- A TOOL_CALL is written BEFORE the tool runs, so a crash in the middle still shows that something may have been
-- written (wrote = true). The outcome follows as a TOOL_RESULT.
CREATE FUNCTION agent_task_events_guard() RETURNS trigger
LANGUAGE plpgsql AS
$$
DECLARE t agent_tasks%ROWTYPE;
BEGIN
  SELECT * INTO t FROM agent_tasks WHERE tenant_id = NEW.tenant_id AND id = NEW.task_id FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'unknown task' USING ERRCODE = 'foreign_key_violation';
  END IF;
  NEW.seq := coalesce((SELECT max(e.seq) FROM agent_task_events e WHERE e.task_id = NEW.task_id), 0) + 1;

  IF NEW.kind = 'STATE' THEN
    NEW.attempt := t.attempt;
    RETURN NEW;
  END IF;
  IF NEW.attempt > t.attempt OR NEW.attempt < 1 THEN
    RAISE EXCEPTION 'task % has no attempt %', t.id, NEW.attempt USING ERRCODE = 'integrity_constraint_violation';
  END IF;

  IF NEW.kind = 'TOOL_RESULT' THEN
    RETURN NEW; -- an outcome is recorded even if the task was cancelled while the call was in flight
  END IF;

  -- NOTE and TOOL_CALL are only accepted from the current attempt of a running task.
  IF t.status <> 'RUNNING' THEN
    RAISE EXCEPTION 'task % is not running (%): it can no longer act', t.id, t.status
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF NEW.attempt <> t.attempt THEN
    RAISE EXCEPTION 'attempt % of task % is over; attempt % is current', NEW.attempt, t.id, t.attempt
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF NEW.kind = 'TOOL_CALL' THEN
    IF t.steps_used >= t.max_steps THEN
      RAISE EXCEPTION 'task % reached its step limit (%)', t.id, t.max_steps USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    UPDATE agent_tasks SET steps_used = steps_used + 1, wrote = wrote OR NOT NEW.read_only
      WHERE tenant_id = t.tenant_id AND id = t.id;
  END IF;
  RETURN NEW;
END
$$;
CREATE TRIGGER agent_task_events_guard BEFORE INSERT ON agent_task_events
  FOR EACH ROW EXECUTE FUNCTION agent_task_events_guard();
CREATE TRIGGER agent_task_events_append_only BEFORE UPDATE OR DELETE ON agent_task_events
  FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- STATE rows come only from this trigger. It is SECURITY DEFINER because the application role may not insert STATE
-- rows itself (policy below), so no one can invent a history.
CREATE FUNCTION agent_tasks_record_state() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS
$$
BEGIN
  IF TG_OP = 'INSERT' THEN
    INSERT INTO agent_task_events (tenant_id, task_id, attempt, kind, detail)
      VALUES (NEW.tenant_id, NEW.id, 0, 'STATE',
              jsonb_build_object('to', NEW.status, 'by', coalesce(nullif(current_setting('app.user_sub', true), ''), NEW.created_by)));
  ELSE
    INSERT INTO agent_task_events (tenant_id, task_id, attempt, kind, detail)
      VALUES (NEW.tenant_id, NEW.id, 0, 'STATE',
              jsonb_build_object('from', OLD.status, 'to', NEW.status, 'by', coalesce(nullif(current_setting('app.user_sub', true), ''), 'system')));
  END IF;
  RETURN NULL;
END
$$;
CREATE TRIGGER agent_tasks_record_state AFTER INSERT ON agent_tasks
  FOR EACH ROW EXECUTE FUNCTION agent_tasks_record_state();
CREATE TRIGGER agent_tasks_record_state_change AFTER UPDATE ON agent_tasks
  FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status) EXECUTE FUNCTION agent_tasks_record_state();

-- ───────────────────────────── agent_schedules ─────────────────────────────
CREATE TABLE agent_schedules (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id      uuid NOT NULL REFERENCES organizations(id),
  agent_key      text NOT NULL,
  goal           text NOT NULL CHECK (char_length(goal) BETWEEN 1 AND 4000),
  every_minutes  int  NOT NULL CHECK (every_minutes BETWEEN 15 AND 10080),   -- cron syntax is deliberately not offered
  enabled        boolean NOT NULL DEFAULT true,
  next_run_at    timestamptz NOT NULL,
  created_by     text NOT NULL CHECK (char_length(created_by) BETWEEN 3 AND 200),
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, agent_key) REFERENCES agent_org_configs (tenant_id, agent_key)
);
CREATE INDEX agent_schedules_due ON agent_schedules (next_run_at) WHERE enabled;
CREATE TRIGGER agent_schedules_touch BEFORE UPDATE ON agent_schedules FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- ───────────────────────────── row-level security ─────────────────────────────
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['agent_tasks','agent_schedules'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
    EXECUTE format('CREATE POLICY tenant_select ON %I FOR SELECT USING (tenant_id = ANY (app_tenant_ids()))', t);
    EXECUTE format('CREATE POLICY tenant_insert ON %I FOR INSERT WITH CHECK (tenant_id = app_tenant_id())', t);
    EXECUTE format('CREATE POLICY tenant_update ON %I FOR UPDATE USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id())', t);
    EXECUTE format('CREATE POLICY tenant_delete ON %I FOR DELETE USING (tenant_id = app_tenant_id())', t);
    EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE ON %I FOR EACH ROW EXECUTE FUNCTION forbid_tenant_change()', t || '_tenant_immutable', t);
  END LOOP;
END
$$;

ALTER TABLE agent_task_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_task_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_select ON agent_task_events FOR SELECT USING (tenant_id = ANY (app_tenant_ids()));
-- The application role writes the bus's own rows only: STATE rows are the trigger's.
CREATE POLICY tenant_insert ON agent_task_events FOR INSERT
  WITH CHECK (tenant_id = app_tenant_id() AND kind <> 'STATE');

-- Cross-tenant work (claiming, expiry, schedules) runs inside SECURITY DEFINER functions owned by venture_owner. FORCE
-- ROW LEVEL SECURITY applies to the owner too, so these policies, which name the owner role and nothing else, are what
-- let those functions (and only code running as the owner) see across tenants. The application role is not the owner
-- and cannot become it.
CREATE POLICY owner_select ON agent_tasks FOR SELECT TO venture_owner USING (true);
CREATE POLICY owner_update ON agent_tasks FOR UPDATE TO venture_owner USING (true) WITH CHECK (true);
CREATE POLICY owner_select ON agent_schedules FOR SELECT TO venture_owner USING (true);
CREATE POLICY owner_select ON agent_task_events FOR SELECT TO venture_owner USING (true);
CREATE POLICY owner_state_insert ON agent_task_events FOR INSERT TO venture_owner WITH CHECK (kind = 'STATE');

-- ───────────────────────────── cross-tenant functions ─────────────────────────────
-- Claim up to p_limit queued tasks, highest priority first, respecting per-tenant and per-agent concurrency. Safe with
-- many dispatchers at once: rows are taken with FOR UPDATE SKIP LOCKED, and a per-tenant advisory lock makes the
-- "how many are running" count and the claim one step. p_now exists so tests can move time.
-- Output columns carry an r_ prefix so they cannot be mistaken for the table's columns.
CREATE FUNCTION agent_task_claim(
  p_owner text,
  p_limit int DEFAULT 1,
  p_now timestamptz DEFAULT now(),
  p_grace interval DEFAULT interval '30 seconds',
  p_max_per_tenant int DEFAULT 2,
  p_max_per_agent int DEFAULT 1)
RETURNS TABLE (r_tenant_id uuid, r_task_id uuid, r_agent_key text, r_attempt smallint, r_run_ref text, r_lease_expires_at timestamptz)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS
$$
#variable_conflict use_column
DECLARE
  c record; n int := 0; running_tenant int; running_agent int;
BEGIN
  -- (the r_ output columns are plpgsql variables here)
  IF p_owner IS NULL OR p_owner = '' THEN RAISE EXCEPTION 'a claim needs an owner name'; END IF;
  IF p_limit < 1 OR p_limit > 100 THEN RAISE EXCEPTION 'p_limit must be between 1 and 100'; END IF;
  FOR c IN
    SELECT t.id, t.tenant_id, t.agent_key, t.max_seconds FROM agent_tasks t
     WHERE t.status = 'QUEUED' AND t.not_before <= p_now
     ORDER BY t.priority, t.not_before, t.created_at
     FOR UPDATE SKIP LOCKED
  LOOP
    EXIT WHEN n >= p_limit;
    PERFORM pg_advisory_xact_lock(hashtextextended('agent_task_claim:' || c.tenant_id::text, 0));
    SELECT count(*) INTO running_tenant FROM agent_tasks r WHERE r.tenant_id = c.tenant_id AND r.status = 'RUNNING';
    IF running_tenant >= p_max_per_tenant THEN CONTINUE; END IF;
    SELECT count(*) INTO running_agent FROM agent_tasks r
      WHERE r.tenant_id = c.tenant_id AND r.agent_key = c.agent_key AND r.status = 'RUNNING';
    IF running_agent >= p_max_per_agent THEN CONTINUE; END IF;

    UPDATE agent_tasks t SET status = 'RUNNING', attempt = t.attempt + 1, lease_owner = p_owner,
           lease_expires_at = p_now + make_interval(secs => c.max_seconds) + p_grace,
           run_ref = t.id::text || '.' || (t.attempt + 1)::text
     WHERE t.id = c.id AND t.tenant_id = c.tenant_id AND t.status = 'QUEUED'
    RETURNING t.tenant_id, t.id, t.agent_key, t.attempt, t.run_ref, t.lease_expires_at
      INTO r_tenant_id, r_task_id, r_agent_key, r_attempt, r_run_ref, r_lease_expires_at;
    RETURN NEXT;
    n := n + 1;
  END LOOP;
END
$$;

-- Tasks whose lease ran out. The caller asks the runtime what became of each, then settles it with a normal
-- tenant-scoped UPDATE that names the attempt it saw.
CREATE FUNCTION agent_task_expired(p_now timestamptz DEFAULT now(), p_limit int DEFAULT 50)
RETURNS TABLE (r_tenant_id uuid, r_task_id uuid, r_attempt smallint, r_run_ref text, r_wrote boolean, r_max_attempts smallint)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS
$$
  SELECT t.tenant_id, t.id, t.attempt, t.run_ref, t.wrote, t.max_attempts FROM agent_tasks t
   WHERE t.status = 'RUNNING' AND t.lease_expires_at < p_now
   ORDER BY t.lease_expires_at LIMIT greatest(1, least(p_limit, 500))
$$;

CREATE FUNCTION agent_schedule_due(p_now timestamptz DEFAULT now(), p_limit int DEFAULT 50)
RETURNS TABLE (r_tenant_id uuid, r_schedule_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public, pg_temp AS
$$
  SELECT s.tenant_id, s.id FROM agent_schedules s
   WHERE s.enabled AND s.next_run_at <= p_now
   ORDER BY s.next_run_at LIMIT greatest(1, least(p_limit, 500))
$$;

-- Fire one schedule, as the tenant (no definer): create the task keyed by schedule and time, and move next_run_at to
-- p_now + every_minutes. A schedule that fell behind therefore runs ONCE, not once per missed slot. If the previous
-- task from this schedule is still waiting, running or needs review, nothing new is created (no pile-up). If the agent
-- can no longer take work (paused, skill not approved) the slot is skipped and the schedule stays enabled.
CREATE FUNCTION agent_schedule_fire(p_schedule uuid, p_now timestamptz DEFAULT now())
RETURNS uuid
LANGUAGE plpgsql AS
$$
DECLARE s agent_schedules%ROWTYPE; busy boolean; tid uuid;
BEGIN
  SELECT * INTO s FROM agent_schedules WHERE id = p_schedule FOR UPDATE;
  IF NOT FOUND OR NOT s.enabled OR s.next_run_at > p_now THEN RETURN NULL; END IF;
  UPDATE agent_schedules SET next_run_at = p_now + make_interval(mins => s.every_minutes) WHERE id = s.id;
  SELECT EXISTS (SELECT 1 FROM agent_tasks WHERE tenant_id = s.tenant_id AND source = 'SCHEDULE' AND source_ref = s.id::text
                  AND status IN ('QUEUED','RUNNING','NEEDS_REVIEW')) INTO busy;
  IF busy THEN RETURN NULL; END IF;
  BEGIN
    INSERT INTO agent_tasks (tenant_id, agent_key, goal, source, source_ref, created_by, idempotency_key)
      VALUES (s.tenant_id, s.agent_key, s.goal, 'SCHEDULE', s.id::text, 'schedule:' || s.id::text,
              'sched:' || s.id::text || ':' || floor(extract(epoch FROM s.next_run_at))::bigint::text)
      ON CONFLICT (tenant_id, idempotency_key) DO NOTHING
      RETURNING id INTO tid;
  EXCEPTION WHEN check_violation THEN
    RETURN NULL;
  END;
  RETURN tid;
END
$$;

-- ───────────────────────────── privileges ─────────────────────────────
REVOKE ALL ON FUNCTION agent_task_claim(text, int, timestamptz, interval, int, int), agent_task_expired(timestamptz, int),
                       agent_schedule_due(timestamptz, int), agent_schedule_fire(uuid, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION agent_task_claim(text, int, timestamptz, interval, int, int), agent_task_expired(timestamptz, int),
                          agent_schedule_due(timestamptz, int), agent_schedule_fire(uuid, timestamptz) TO venture_app;
GRANT SELECT, INSERT, UPDATE ON agent_tasks TO venture_app;           -- never DELETE: history is kept
GRANT SELECT, INSERT         ON agent_task_events TO venture_app;     -- append-only
GRANT SELECT, INSERT, UPDATE, DELETE ON agent_schedules TO venture_app;

INSERT INTO schema_version (version) VALUES (5);
COMMIT;

-- Venture OS core schema (PostgreSQL 15+; developed against 17).
--
-- Applied once, as role `venture_owner`, by db/apply.sql (compose service `schema`). It does not create roles or
-- extensions: infra/postgres/init/01-bootstrap.sh creates `venture_owner` / `venture_app`.
--
-- ISOLATION MODEL
--   * Every table carries tenant_id (an organizations.id). Row-level security is ENABLEd and FORCEd on all of
--     them, so even the owning role is filtered; `venture_app` is NOSUPERUSER NOBYPASSRLS.
--   * A request sets, with SET LOCAL (i.e. set_config(..., true)) inside its transaction:
--         app.tenant_id   the single tenant a request acts in. REQUIRED for any write.
--         app.tenant_ids  optional comma-separated uuid list: tenants the caller may READ across
--                         (portfolio overview, global approval inbox). Never grants write access.
--         app.user_sub    the authenticated OIDC subject; lets a user discover their own memberships.
--   * Nothing set => every policy evaluates to false => zero rows / writes rejected (fail closed).
--   * Cross-table links are composite (tenant_id, id) foreign keys, so a row can never reference another
--     tenant's row even if application code is wrong.
--   * Append-only tables (ledger, audit) and state machines (approvals, blueprints) are enforced by triggers and
--     by granting venture_app only the privileges it needs: the application cannot rewrite history.
--
-- IDEMPOTENCY
--   Every table that records a command has idempotency_key with UNIQUE (tenant_id, idempotency_key). A retried
--   command conflicts instead of duplicating; `idempotency_keys` additionally stores API-level replay responses.
--
-- MONEY is always bigint minor units (cents, paise, …) plus an ISO-4217 currency; there are no floats.

BEGIN;

-- ───────────────────────────── session helpers ─────────────────────────────
CREATE FUNCTION app_tenant_id() RETURNS uuid
LANGUAGE sql STABLE AS
$$ SELECT nullif(current_setting('app.tenant_id', true), '')::uuid $$;

CREATE FUNCTION app_tenant_ids() RETURNS uuid[]
LANGUAGE sql STABLE AS
$$
  SELECT CASE
    WHEN nullif(current_setting('app.tenant_ids', true), '') IS NOT NULL
      THEN string_to_array(current_setting('app.tenant_ids', true), ',')::uuid[]
    WHEN nullif(current_setting('app.tenant_id', true), '') IS NOT NULL
      THEN ARRAY[current_setting('app.tenant_id', true)::uuid]
    ELSE ARRAY[]::uuid[]
  END
$$;

CREATE FUNCTION app_user_sub() RETURNS text
LANGUAGE sql STABLE AS
$$ SELECT nullif(current_setting('app.user_sub', true), '') $$;

CREATE FUNCTION touch_updated_at() RETURNS trigger
LANGUAGE plpgsql AS
$$ BEGIN NEW.updated_at := now(); RETURN NEW; END $$;

CREATE FUNCTION forbid_tenant_change() RETURNS trigger
LANGUAGE plpgsql AS
$$
BEGIN
  IF TG_TABLE_NAME = 'organizations' THEN
    IF NEW.id IS DISTINCT FROM OLD.id THEN
      RAISE EXCEPTION 'organizations.id is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
  ELSIF to_jsonb(NEW)->>'tenant_id' IS DISTINCT FROM to_jsonb(OLD)->>'tenant_id' THEN
    RAISE EXCEPTION '%.tenant_id is immutable', TG_TABLE_NAME USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END
$$;

CREATE FUNCTION forbid_mutation() RETURNS trigger
LANGUAGE plpgsql AS
$$
BEGIN
  RAISE EXCEPTION '% is append-only: % rejected', TG_TABLE_NAME, TG_OP USING ERRCODE = 'integrity_constraint_violation';
END
$$;

-- ───────────────────────────── organizations (= tenants) ─────────────────────────────
CREATE TABLE organizations (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  slug             text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$'),
  name             text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
  industry         text,
  country          char(2) CHECK (country ~ '^[A-Z]{2}$'),
  base_currency    char(3) NOT NULL DEFAULT 'USD' CHECK (base_currency ~ '^[A-Z]{3}$'),
  timezone         text NOT NULL DEFAULT 'UTC',
  status           text NOT NULL DEFAULT 'DRAFT'
                   CHECK (status IN ('DRAFT','PROVISIONING','ACTIVE','SUSPENDED','ARCHIVED')),
  created_by       text NOT NULL,
  idempotency_key  text NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (created_by, idempotency_key)
);
CREATE TRIGGER organizations_touch BEFORE UPDATE ON organizations
  FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
CREATE TRIGGER organizations_id_immutable BEFORE UPDATE ON organizations
  FOR EACH ROW EXECUTE FUNCTION forbid_tenant_change();

CREATE TABLE tenant_members (
  tenant_id   uuid NOT NULL REFERENCES organizations(id),
  user_sub    text NOT NULL,
  role        text NOT NULL CHECK (role IN ('founder','approver','viewer')),
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, user_sub)
);
CREATE INDEX tenant_members_user_idx ON tenant_members (user_sub);

-- Which external system holds which tenant's data. No secrets here: secret_ref names an entry in a secret store.
CREATE TABLE tenant_integrations (
  tenant_id    uuid NOT NULL REFERENCES organizations(id),
  system       text NOT NULL CHECK (system IN ('keycloak','twenty','libredesk','erpnext','hermes')),
  external_id  text NOT NULL,
  config       jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(config) = 'object'),
  secret_ref   text,
  status       text NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE','DEGRADED','REMOVED')),
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, system)
);
CREATE TRIGGER tenant_integrations_touch BEFORE UPDATE ON tenant_integrations
  FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- ───────────────────────────── genesis (conversation → blueprint) ─────────────────────────────
CREATE TABLE genesis_sessions (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL REFERENCES organizations(id),
  status           text NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE','COMPLETED','ABANDONED')),
  draft_blueprint  jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(draft_blueprint) = 'object'),
  created_by       text NOT NULL,
  idempotency_key  text NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, idempotency_key)
);
CREATE TRIGGER genesis_sessions_touch BEFORE UPDATE ON genesis_sessions
  FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE genesis_messages (
  id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id        uuid NOT NULL,
  session_id       uuid NOT NULL,
  role             text NOT NULL CHECK (role IN ('founder','assistant','system','tool')),
  content          text NOT NULL,
  idempotency_key  text NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, session_id) REFERENCES genesis_sessions (tenant_id, id),
  UNIQUE (tenant_id, idempotency_key)
);
CREATE INDEX genesis_messages_session_idx ON genesis_messages (tenant_id, session_id, id);

-- ───────────────────────────── company blueprints ─────────────────────────────
CREATE TABLE company_blueprints (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL REFERENCES organizations(id),
  version          integer NOT NULL CHECK (version > 0),
  status           text NOT NULL DEFAULT 'DRAFT' CHECK (status IN ('DRAFT','APPROVED','SUPERSEDED','REJECTED')),
  schema_version   text NOT NULL,
  blueprint        jsonb NOT NULL CHECK (jsonb_typeof(blueprint) = 'object'),
  content_hash     bytea NOT NULL,                      -- sha256 of the blueprint; maintained by trigger
  session_id       uuid,
  created_by       text NOT NULL,
  approved_by      text,
  approved_at      timestamptz,
  idempotency_key  text NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, version),
  UNIQUE (tenant_id, idempotency_key),
  FOREIGN KEY (tenant_id, session_id) REFERENCES genesis_sessions (tenant_id, id),
  CHECK (status <> 'APPROVED' OR (approved_by IS NOT NULL AND approved_at IS NOT NULL))
);
-- At most one live (approved) blueprint per tenant.
CREATE UNIQUE INDEX company_blueprints_one_approved ON company_blueprints (tenant_id) WHERE status = 'APPROVED';

CREATE FUNCTION company_blueprints_guard() RETURNS trigger
LANGUAGE plpgsql AS
$$
BEGIN
  IF TG_OP = 'UPDATE' THEN
    IF OLD.status <> 'DRAFT' AND NEW.blueprint IS DISTINCT FROM OLD.blueprint THEN
      RAISE EXCEPTION 'blueprint % is frozen (status %)', OLD.id, OLD.status
        USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.status <> OLD.status AND NOT (
         (OLD.status = 'DRAFT'    AND NEW.status IN ('APPROVED','REJECTED')) OR
         (OLD.status = 'APPROVED' AND NEW.status = 'SUPERSEDED')) THEN
      RAISE EXCEPTION 'illegal blueprint transition % -> %', OLD.status, NEW.status
        USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    NEW.updated_at := now();
  END IF;
  NEW.content_hash := sha256(convert_to(NEW.blueprint::text, 'UTF8'));
  RETURN NEW;
END
$$;
CREATE TRIGGER company_blueprints_guard BEFORE INSERT OR UPDATE ON company_blueprints
  FOR EACH ROW EXECUTE FUNCTION company_blueprints_guard();

-- ───────────────────────────── provisioner (saga state) ─────────────────────────────
CREATE TABLE provision_runs (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL,
  blueprint_id     uuid NOT NULL,
  status           text NOT NULL DEFAULT 'PENDING'
                   CHECK (status IN ('PENDING','RUNNING','SUCCEEDED','FAILED','COMPENSATING','ROLLED_BACK','COMPENSATION_FAILED')),
  error            text,
  started_at       timestamptz,
  finished_at      timestamptz,
  idempotency_key  text NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, idempotency_key),
  FOREIGN KEY (tenant_id, blueprint_id) REFERENCES company_blueprints (tenant_id, id)
);
-- A tenant has at most one in-flight provisioning run.
CREATE UNIQUE INDEX provision_runs_one_active ON provision_runs (tenant_id)
  WHERE status IN ('PENDING','RUNNING','COMPENSATING');
CREATE TRIGGER provision_runs_touch BEFORE UPDATE ON provision_runs
  FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE provision_steps (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL,
  run_id           uuid NOT NULL,
  step_key         text NOT NULL,
  ordinal          integer NOT NULL CHECK (ordinal >= 0),
  status           text NOT NULL DEFAULT 'PENDING'
                   CHECK (status IN ('PENDING','RUNNING','SUCCEEDED','FAILED','COMPENSATED','COMPENSATION_FAILED','SKIPPED')),
  attempts         integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  external_ref     jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(external_ref) = 'object'),
  error            text,
  started_at       timestamptz,
  finished_at      timestamptz,
  compensated_at   timestamptz,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, run_id) REFERENCES provision_runs (tenant_id, id),
  UNIQUE (run_id, step_key),
  UNIQUE (run_id, ordinal)
);
CREATE TRIGGER provision_steps_touch BEFORE UPDATE ON provision_steps
  FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- ───────────────────────────── finance: pricing rules ─────────────────────────────
CREATE TABLE tenant_pricing_rules (
  id                         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id                  uuid NOT NULL REFERENCES organizations(id),
  name                       text NOT NULL,
  version                    integer NOT NULL DEFAULT 1 CHECK (version > 0),
  model                      text NOT NULL CHECK (model IN ('SAAS_TIER','D2C_UNIT','EXIM_LANDED_COST','CUSTOM')),
  currency                   char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  formula                    jsonb NOT NULL CHECK (jsonb_typeof(formula) = 'object'),
  margin_floor_bps           integer NOT NULL CHECK (margin_floor_bps BETWEEN 0 AND 10000),
  margin_target_bps          integer CHECK (margin_target_bps BETWEEN 0 AND 10000),
  approval_threshold_minor   bigint CHECK (approval_threshold_minor >= 0),
  status                     text NOT NULL DEFAULT 'DRAFT' CHECK (status IN ('DRAFT','ACTIVE','RETIRED')),
  blueprint_id               uuid,
  created_by                 text NOT NULL,
  idempotency_key            text NOT NULL,
  created_at                 timestamptz NOT NULL DEFAULT now(),
  updated_at                 timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, name, version),
  UNIQUE (tenant_id, idempotency_key),
  FOREIGN KEY (tenant_id, blueprint_id) REFERENCES company_blueprints (tenant_id, id),
  CHECK (margin_target_bps IS NULL OR margin_target_bps >= margin_floor_bps)
);
CREATE UNIQUE INDEX tenant_pricing_rules_one_active ON tenant_pricing_rules (tenant_id, name) WHERE status = 'ACTIVE';
CREATE TRIGGER tenant_pricing_rules_touch BEFORE UPDATE ON tenant_pricing_rules
  FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- ───────────────────────────── finance: double-entry ledger ─────────────────────────────
CREATE TABLE ledger_accounts (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL REFERENCES organizations(id),
  code        text NOT NULL CHECK (code ~ '^[A-Za-z0-9._-]{1,32}$'),
  name        text NOT NULL,
  type        text NOT NULL CHECK (type IN ('ASSET','LIABILITY','EQUITY','REVENUE','EXPENSE')),
  normal_side text GENERATED ALWAYS AS (CASE WHEN type IN ('ASSET','EXPENSE') THEN 'DEBIT' ELSE 'CREDIT' END) STORED,
  parent_id   uuid,
  currency    char(3) CHECK (currency ~ '^[A-Z]{3}$'),     -- NULL = any currency
  is_postable boolean NOT NULL DEFAULT true,
  active      boolean NOT NULL DEFAULT true,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, code),
  FOREIGN KEY (tenant_id, parent_id) REFERENCES ledger_accounts (tenant_id, id),
  CHECK (parent_id IS NULL OR parent_id <> id)
);
CREATE TRIGGER ledger_accounts_touch BEFORE UPDATE ON ledger_accounts
  FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- A journal is a balanced set of entries, posted atomically. entry_count pins the number of lines so nothing can
-- be appended to a posted journal later.
CREATE TABLE ledger_journals (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL REFERENCES organizations(id),
  entry_count      integer NOT NULL CHECK (entry_count >= 2),
  effective_date   date NOT NULL DEFAULT current_date,
  memo             text NOT NULL DEFAULT '',
  source_type      text,
  source_id        text,
  reversal_of      uuid,
  created_by       text NOT NULL,
  idempotency_key  text NOT NULL,
  posted_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, idempotency_key),
  FOREIGN KEY (tenant_id, reversal_of) REFERENCES ledger_journals (tenant_id, id)
);

CREATE TABLE ledger_entries (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id     uuid NOT NULL,
  journal_id    uuid NOT NULL,
  line_no       integer NOT NULL CHECK (line_no > 0),
  account_id    uuid NOT NULL,
  debit_minor   bigint NOT NULL DEFAULT 0 CHECK (debit_minor >= 0),
  credit_minor  bigint NOT NULL DEFAULT 0 CHECK (credit_minor >= 0),
  currency      char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  memo          text,
  FOREIGN KEY (tenant_id, journal_id) REFERENCES ledger_journals (tenant_id, id),
  FOREIGN KEY (tenant_id, account_id) REFERENCES ledger_accounts (tenant_id, id),
  UNIQUE (journal_id, line_no),
  CHECK ((debit_minor > 0) <> (credit_minor > 0))          -- exactly one side, strictly positive
);
CREATE INDEX ledger_entries_account_idx ON ledger_entries (tenant_id, account_id);

CREATE FUNCTION ledger_check_journal() RETURNS trigger
LANGUAGE plpgsql AS
$$
DECLARE
  j uuid;
  declared integer;
  actual integer;
BEGIN
  IF TG_TABLE_NAME = 'ledger_journals' THEN
    j := NEW.id;
  ELSE
    j := NEW.journal_id;
  END IF;
  SELECT entry_count INTO declared FROM ledger_journals WHERE id = j;
  SELECT count(*) INTO actual FROM ledger_entries WHERE journal_id = j;
  IF actual <> declared THEN
    RAISE EXCEPTION 'journal % declares % entries but has %', j, declared, actual
      USING ERRCODE = 'check_violation';
  END IF;
  IF EXISTS (SELECT 1 FROM ledger_entries WHERE journal_id = j
             GROUP BY currency HAVING sum(debit_minor) <> sum(credit_minor)) THEN
    RAISE EXCEPTION 'journal % is not balanced per currency', j USING ERRCODE = 'check_violation';
  END IF;
  IF EXISTS (SELECT 1 FROM ledger_entries e JOIN ledger_accounts a
               ON a.tenant_id = e.tenant_id AND a.id = e.account_id
             WHERE e.journal_id = j
               AND (NOT a.is_postable OR NOT a.active OR (a.currency IS NOT NULL AND a.currency <> e.currency))) THEN
    RAISE EXCEPTION 'journal % posts to a non-postable, inactive or wrong-currency account', j
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER ledger_journals_balanced AFTER INSERT ON ledger_journals
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger_check_journal();
CREATE CONSTRAINT TRIGGER ledger_entries_balanced AFTER INSERT ON ledger_entries
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger_check_journal();
CREATE TRIGGER ledger_journals_append_only BEFORE UPDATE OR DELETE ON ledger_journals
  FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER ledger_entries_append_only BEFORE UPDATE OR DELETE ON ledger_entries
  FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Forecast / committed / settled cash movements feeding the runway and working-capital views.
CREATE TABLE cashflow_items (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL REFERENCES organizations(id),
  direction        text NOT NULL CHECK (direction IN ('INFLOW','OUTFLOW')),
  category         text NOT NULL,
  amount_minor     bigint NOT NULL CHECK (amount_minor > 0),
  currency         char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  due_date         date NOT NULL,
  status           text NOT NULL DEFAULT 'FORECAST' CHECK (status IN ('FORECAST','COMMITTED','SETTLED','CANCELLED')),
  source_type      text,
  source_id        text,
  memo             text NOT NULL DEFAULT '',
  idempotency_key  text NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, idempotency_key)
);
CREATE INDEX cashflow_items_due_idx ON cashflow_items (tenant_id, due_date) WHERE status IN ('FORECAST','COMMITTED');
CREATE TRIGGER cashflow_items_touch BEFORE UPDATE ON cashflow_items
  FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- ───────────────────────────── CRM pipelines & workflows ─────────────────────────────
CREATE TABLE crm_pipelines (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL REFERENCES organizations(id),
  key              text NOT NULL CHECK (key ~ '^[a-z][a-z0-9_]{1,62}$'),
  name             text NOT NULL,
  entity_type      text NOT NULL CHECK (entity_type IN ('LEAD','DEAL','ORDER','TICKET','CUSTOM')),
  stages           jsonb NOT NULL CHECK (jsonb_typeof(stages) = 'array' AND jsonb_array_length(stages) > 0),
  custom_fields    jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(custom_fields) = 'array'),
  external_ref     text,                                    -- id in Twenty once provisioned
  status           text NOT NULL DEFAULT 'DRAFT' CHECK (status IN ('DRAFT','ACTIVE','RETIRED')),
  blueprint_id     uuid,
  idempotency_key  text NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, key),
  UNIQUE (tenant_id, idempotency_key),
  FOREIGN KEY (tenant_id, blueprint_id) REFERENCES company_blueprints (tenant_id, id)
);
CREATE TRIGGER crm_pipelines_touch BEFORE UPDATE ON crm_pipelines
  FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE workflows (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id          uuid NOT NULL REFERENCES organizations(id),
  key                text NOT NULL CHECK (key ~ '^[a-z][a-z0-9_]{1,62}$'),
  version            integer NOT NULL DEFAULT 1 CHECK (version > 0),
  name               text NOT NULL,
  description        text NOT NULL DEFAULT '',
  trigger            jsonb NOT NULL CHECK (jsonb_typeof(trigger) = 'object'),
  definition         jsonb NOT NULL CHECK (jsonb_typeof(definition) = 'object'),
  requires_approval  boolean NOT NULL DEFAULT true,
  status             text NOT NULL DEFAULT 'DRAFT' CHECK (status IN ('DRAFT','ACTIVE','PAUSED','RETIRED')),
  blueprint_id       uuid,
  idempotency_key    text NOT NULL,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, key, version),
  UNIQUE (tenant_id, idempotency_key),
  FOREIGN KEY (tenant_id, blueprint_id) REFERENCES company_blueprints (tenant_id, id)
);
CREATE TRIGGER workflows_touch BEFORE UPDATE ON workflows
  FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- ───────────────────────────── governance: approval queue ─────────────────────────────
CREATE TABLE approval_queue (
  id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id            uuid NOT NULL REFERENCES organizations(id),
  kind                 text NOT NULL CHECK (kind IN
                         ('QUOTE','INVOICE','PURCHASE_ORDER','PAYMENT','CAMPAIGN','AGENT_SKILL','BLUEPRINT','OTHER')),
  status               text NOT NULL DEFAULT 'PENDING_APPROVAL' CHECK (status IN
                         ('PENDING_APPROVAL','APPROVED','REJECTED','EXECUTING','EXECUTED','FAILED','EXPIRED','CANCELLED')),
  title                text NOT NULL CHECK (length(btrim(title)) > 0),
  summary              text NOT NULL DEFAULT '',
  requested_by_type    text NOT NULL CHECK (requested_by_type IN ('AGENT','USER','SYSTEM')),
  requested_by         text NOT NULL,
  risk_level           text NOT NULL DEFAULT 'MEDIUM' CHECK (risk_level IN ('LOW','MEDIUM','HIGH')),
  payload              jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
  payload_hash         bytea NOT NULL,                       -- sha256(payload); executor re-verifies before acting
  amount_minor         bigint CHECK (amount_minor >= 0),
  currency             char(3) CHECK (currency ~ '^[A-Z]{3}$'),
  related_entity_type  text,
  related_entity_id    text,
  decided_by           text,
  decided_at           timestamptz,
  decision_note        text,
  executed_at          timestamptz,
  execution_result     jsonb,
  expires_at           timestamptz,
  idempotency_key      text NOT NULL,
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, idempotency_key),
  CHECK ((amount_minor IS NULL) = (currency IS NULL)),
  CHECK (status IN ('PENDING_APPROVAL','EXPIRED','CANCELLED')
         OR (decided_by IS NOT NULL AND decided_at IS NOT NULL))
);
CREATE INDEX approval_queue_inbox_idx ON approval_queue (tenant_id, created_at DESC) WHERE status = 'PENDING_APPROVAL';
CREATE INDEX approval_queue_status_idx ON approval_queue (tenant_id, status, created_at DESC);

CREATE FUNCTION approval_queue_guard() RETURNS trigger
LANGUAGE plpgsql AS
$$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'PENDING_APPROVAL' THEN
      RAISE EXCEPTION 'an approval must be created as PENDING_APPROVAL' USING ERRCODE = 'check_violation';
    END IF;
    NEW.payload_hash := sha256(convert_to(NEW.payload::text, 'UTF8'));
    RETURN NEW;
  END IF;

  -- What was asked for can never change after the fact: the founder approves exactly this payload.
  IF NEW.payload IS DISTINCT FROM OLD.payload OR NEW.kind <> OLD.kind
     OR NEW.amount_minor IS DISTINCT FROM OLD.amount_minor OR NEW.currency IS DISTINCT FROM OLD.currency
     OR NEW.requested_by <> OLD.requested_by OR NEW.requested_by_type <> OLD.requested_by_type
     OR NEW.idempotency_key <> OLD.idempotency_key THEN
    RAISE EXCEPTION 'approval % request is immutable', OLD.id USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF OLD.decided_by IS NOT NULL AND NEW.decided_by IS DISTINCT FROM OLD.decided_by THEN
    RAISE EXCEPTION 'approval % decision is immutable', OLD.id USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  NEW.payload_hash := OLD.payload_hash;

  IF NEW.status <> OLD.status AND NOT (
       (OLD.status = 'PENDING_APPROVAL' AND NEW.status IN ('APPROVED','REJECTED','EXPIRED','CANCELLED')) OR
       (OLD.status = 'APPROVED'         AND NEW.status IN ('EXECUTING','CANCELLED')) OR
       (OLD.status = 'EXECUTING'        AND NEW.status IN ('EXECUTED','FAILED')) OR
       (OLD.status = 'FAILED'           AND NEW.status = 'EXECUTING')) THEN
    RAISE EXCEPTION 'illegal approval transition % -> %', OLD.status, NEW.status
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF OLD.status IN ('REJECTED','EXECUTED','EXPIRED','CANCELLED') THEN
    RAISE EXCEPTION 'approval % is final (%)', OLD.id, OLD.status USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  NEW.updated_at := now();
  RETURN NEW;
END
$$;
CREATE TRIGGER approval_queue_guard BEFORE INSERT OR UPDATE ON approval_queue
  FOR EACH ROW EXECUTE FUNCTION approval_queue_guard();

CREATE TABLE workflow_runs (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL,
  workflow_id      uuid NOT NULL,
  status           text NOT NULL DEFAULT 'PENDING' CHECK (status IN
                     ('PENDING','RUNNING','AWAITING_APPROVAL','SUCCEEDED','FAILED','CANCELLED')),
  state            jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(state) = 'object'),
  approval_id      uuid,
  error            text,
  started_at       timestamptz,
  finished_at      timestamptz,
  idempotency_key  text NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, idempotency_key),
  FOREIGN KEY (tenant_id, workflow_id) REFERENCES workflows (tenant_id, id),
  FOREIGN KEY (tenant_id, approval_id) REFERENCES approval_queue (tenant_id, id)
);
CREATE TRIGGER workflow_runs_touch BEFORE UPDATE ON workflow_runs
  FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- ───────────────────────────── agents ─────────────────────────────
CREATE TABLE agent_org_configs (
  id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id             uuid NOT NULL REFERENCES organizations(id),
  agent_key             text NOT NULL CHECK (agent_key ~ '^[a-z][a-z0-9_-]{1,62}$'),
  role_title            text NOT NULL,
  reports_to_agent_key  text,
  runtime               text NOT NULL DEFAULT 'hermes' CHECK (runtime IN ('hermes')),
  skill_md              text NOT NULL,
  skill_sha256          bytea NOT NULL,                      -- maintained by trigger; Hermes installs only APPROVED hashes
  skill_status          text NOT NULL DEFAULT 'DRAFT' CHECK (skill_status IN ('DRAFT','PENDING_APPROVAL','APPROVED','REJECTED')),
  tools_allowed         text[] NOT NULL DEFAULT '{}',
  monthly_budget_minor  bigint NOT NULL DEFAULT 0 CHECK (monthly_budget_minor >= 0),
  budget_currency       char(3) NOT NULL DEFAULT 'USD' CHECK (budget_currency ~ '^[A-Z]{3}$'),
  status                text NOT NULL DEFAULT 'PLANNED' CHECK (status IN ('PLANNED','ACTIVE','PAUSED','RETIRED')),
  hermes_profile        text,
  blueprint_id          uuid,
  idempotency_key       text NOT NULL,
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, agent_key),
  UNIQUE (tenant_id, idempotency_key),
  FOREIGN KEY (tenant_id, reports_to_agent_key) REFERENCES agent_org_configs (tenant_id, agent_key)
    DEFERRABLE INITIALLY DEFERRED,
  FOREIGN KEY (tenant_id, blueprint_id) REFERENCES company_blueprints (tenant_id, id),
  CHECK (reports_to_agent_key IS NULL OR reports_to_agent_key <> agent_key)
);

CREATE FUNCTION agent_org_configs_guard() RETURNS trigger
LANGUAGE plpgsql AS
$$
BEGIN
  IF TG_OP = 'UPDATE' THEN
    -- Changed skill text may not stay APPROVED: it has to go back through the approval queue.
    IF NEW.skill_md IS DISTINCT FROM OLD.skill_md AND NEW.skill_status NOT IN ('DRAFT','PENDING_APPROVAL') THEN
      RAISE EXCEPTION 'changing skill_md requires skill_status DRAFT or PENDING_APPROVAL'
        USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    NEW.updated_at := now();
  END IF;
  NEW.skill_sha256 := sha256(convert_to(NEW.skill_md, 'UTF8'));
  RETURN NEW;
END
$$;
CREATE TRIGGER agent_org_configs_guard BEFORE INSERT OR UPDATE ON agent_org_configs
  FOR EACH ROW EXECUTE FUNCTION agent_org_configs_guard();

-- ───────────────────────────── audit & idempotency ─────────────────────────────
CREATE TABLE audit_logs (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant_id    uuid NOT NULL REFERENCES organizations(id),
  occurred_at  timestamptz NOT NULL DEFAULT now(),
  actor_type   text NOT NULL CHECK (actor_type IN ('USER','AGENT','SYSTEM')),
  actor_id     text NOT NULL,
  action       text NOT NULL,
  entity_type  text,
  entity_id    text,
  outcome      text NOT NULL DEFAULT 'SUCCESS' CHECK (outcome IN ('SUCCESS','DENIED','ERROR')),
  request_id   text,
  data         jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(data) = 'object')
);
CREATE INDEX audit_logs_tenant_time_idx ON audit_logs (tenant_id, occurred_at DESC);
CREATE INDEX audit_logs_entity_idx ON audit_logs (tenant_id, entity_type, entity_id);
CREATE TRIGGER audit_logs_append_only BEFORE UPDATE OR DELETE ON audit_logs
  FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE idempotency_keys (
  tenant_id     uuid NOT NULL REFERENCES organizations(id),
  key           text NOT NULL,
  request_hash  bytea NOT NULL,
  status        text NOT NULL DEFAULT 'IN_PROGRESS' CHECK (status IN ('IN_PROGRESS','COMPLETED')),
  response      jsonb,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL DEFAULT now() + interval '48 hours',
  PRIMARY KEY (tenant_id, key)
);
CREATE INDEX idempotency_keys_expiry_idx ON idempotency_keys (expires_at);

-- ───────────────────────────── row-level security ─────────────────────────────
-- Reads span app.tenant_ids (or the single app.tenant_id); every write is confined to app.tenant_id.
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY[
    'tenant_integrations','genesis_sessions','genesis_messages','company_blueprints','provision_runs',
    'provision_steps','tenant_pricing_rules','ledger_accounts','ledger_journals','ledger_entries',
    'cashflow_items','crm_pipelines','workflows','workflow_runs','approval_queue','agent_org_configs',
    'audit_logs','idempotency_keys'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
    EXECUTE format('CREATE POLICY tenant_select ON %I FOR SELECT USING (tenant_id = ANY (app_tenant_ids()))', t);
    EXECUTE format('CREATE POLICY tenant_insert ON %I FOR INSERT WITH CHECK (tenant_id = app_tenant_id())', t);
    EXECUTE format('CREATE POLICY tenant_update ON %I FOR UPDATE USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id())', t);
    EXECUTE format('CREATE POLICY tenant_delete ON %I FOR DELETE USING (tenant_id = app_tenant_id())', t);
    -- tenant_id never changes (tables without UPDATE grants never reach this, which is fine).
    EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE ON %I FOR EACH ROW EXECUTE FUNCTION forbid_tenant_change()',
                   t || '_tenant_immutable', t);
  END LOOP;
END
$$;

ALTER TABLE organizations ENABLE ROW LEVEL SECURITY;
ALTER TABLE organizations FORCE ROW LEVEL SECURITY;
-- A tenant is created by generating its uuid, setting app.tenant_id to it, then inserting the organization and
-- its first tenant_members row in one transaction.
CREATE POLICY org_select ON organizations FOR SELECT USING (id = ANY (app_tenant_ids()));
CREATE POLICY org_insert ON organizations FOR INSERT WITH CHECK (id = app_tenant_id());
CREATE POLICY org_update ON organizations FOR UPDATE USING (id = app_tenant_id()) WITH CHECK (id = app_tenant_id());

ALTER TABLE tenant_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_members FORCE ROW LEVEL SECURITY;
-- A user may list their own memberships (to discover which tenants to put in app.tenant_ids).
CREATE POLICY members_select ON tenant_members FOR SELECT
  USING (tenant_id = ANY (app_tenant_ids()) OR user_sub = app_user_sub());
CREATE POLICY members_insert ON tenant_members FOR INSERT WITH CHECK (tenant_id = app_tenant_id());
CREATE POLICY members_update ON tenant_members FOR UPDATE
  USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id());
CREATE POLICY members_delete ON tenant_members FOR DELETE USING (tenant_id = app_tenant_id());
CREATE TRIGGER tenant_members_tenant_immutable BEFORE UPDATE ON tenant_members
  FOR EACH ROW EXECUTE FUNCTION forbid_tenant_change();

-- ───────────────────────────── privileges (least privilege, explicit) ─────────────────────────────
-- venture_app never gets TRUNCATE, DDL, or UPDATE/DELETE on append-only data.
GRANT EXECUTE ON FUNCTION app_tenant_id(), app_tenant_ids(), app_user_sub() TO venture_app;
GRANT SELECT, INSERT, UPDATE         ON organizations, company_blueprints, genesis_sessions, provision_runs,
                                        provision_steps, tenant_pricing_rules, ledger_accounts, cashflow_items,
                                        workflows, workflow_runs, approval_queue TO venture_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_members, tenant_integrations, crm_pipelines, agent_org_configs,
                                        idempotency_keys TO venture_app;
GRANT SELECT, INSERT                 ON genesis_messages, ledger_journals, ledger_entries, audit_logs TO venture_app;

-- ───────────────────────────── bookkeeping ─────────────────────────────
CREATE TABLE schema_version (
  version     integer PRIMARY KEY,
  applied_at  timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_version (version) VALUES (1);
GRANT SELECT ON schema_version TO venture_app;

COMMIT;

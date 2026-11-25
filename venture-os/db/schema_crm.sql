-- Venture OS built-in CRM (schema version 2). Applied as venture_owner after schema.sql by db/apply.sql.
--
-- Same isolation model as schema.sql: every table carries tenant_id, RLS is ENABLEd and FORCEd, cross-table links are
-- composite (tenant_id, id) foreign keys, creates are idempotent per (tenant_id, idempotency_key), and venture_app
-- gets only the privileges it needs. There is no DELETE anywhere: records are archived, never removed.
--
-- DESIGN NOTES
--   * `seq` is an identity column used only to order lists oldest-created first and as the paging cursor. Ordering by
--     a timestamp would not be stable (rows created in one transaction share now()), and uuids are random.
--     Caveat: an identity value is taken at INSERT but becomes visible at COMMIT, so a row whose transaction commits
--     after a later one can be missed by a page iteration already past it. Fine for interactive lists (they refresh);
--     do not build an exporter on this cursor.
--   * A deal's pipeline_key/stage are validated by the application against crm_pipelines (stages are jsonb, so they
--     cannot be a foreign key). There is deliberately no foreign key to crm_pipelines either: re-provisioning with a
--     changed blueprint may drop or rename a pipeline, and that must not be blocked by, or destroy, existing deals.
--   * Search is ILIKE '%…%' with sequential scans: right for thousands of rows per tenant. Add pg_trgm indexes in a
--     later migration if a tenant outgrows it (PGlite, used by tests, has no extensions).
--   * Free text limits are in characters (char_length), matching internal/crm validation.

BEGIN;

-- Pipelines keep the order the blueprint declares them in.
ALTER TABLE crm_pipelines ADD COLUMN position integer NOT NULL DEFAULT 0;

-- ───────────────────────────── companies ─────────────────────────────
CREATE TABLE crm_companies (
  seq              bigint GENERATED ALWAYS AS IDENTITY,
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL REFERENCES organizations(id),
  name             text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
  domain           text NOT NULL DEFAULT '' CHECK (char_length(domain) <= 253),
  industry         text NOT NULL DEFAULT '' CHECK (char_length(industry) <= 100),
  country          text NOT NULL DEFAULT '' CHECK (country ~ '^([A-Z]{2})?$'),
  website          text NOT NULL DEFAULT '' CHECK (char_length(website) <= 500),
  phone            text NOT NULL DEFAULT '' CHECK (char_length(phone) <= 50),
  owner_sub        text NOT NULL DEFAULT '' CHECK (char_length(owner_sub) <= 200),
  fields           jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(fields) = 'object'),
  version          bigint NOT NULL DEFAULT 1,
  idempotency_key  text NOT NULL,
  request_hash     bytea NOT NULL,
  archived_at      timestamptz,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, idempotency_key),
  UNIQUE (tenant_id, seq)
);
CREATE INDEX crm_companies_owner_idx ON crm_companies (tenant_id, owner_sub) WHERE archived_at IS NULL;

-- ───────────────────────────── contacts ─────────────────────────────
CREATE TABLE crm_contacts (
  seq              bigint GENERATED ALWAYS AS IDENTITY,
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL REFERENCES organizations(id),
  company_id       uuid,
  first_name       text NOT NULL DEFAULT '' CHECK (char_length(first_name) <= 100),
  last_name        text NOT NULL DEFAULT '' CHECK (char_length(last_name) <= 100),
  email            text NOT NULL DEFAULT '' CHECK (char_length(email) <= 254),
  phone            text NOT NULL DEFAULT '' CHECK (char_length(phone) <= 50),
  title            text NOT NULL DEFAULT '' CHECK (char_length(title) <= 100),
  owner_sub        text NOT NULL DEFAULT '' CHECK (char_length(owner_sub) <= 200),
  fields           jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(fields) = 'object'),
  version          bigint NOT NULL DEFAULT 1,
  idempotency_key  text NOT NULL,
  request_hash     bytea NOT NULL,
  archived_at      timestamptz,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, idempotency_key),
  UNIQUE (tenant_id, seq),
  CHECK (first_name <> '' OR last_name <> '' OR email <> ''),
  FOREIGN KEY (tenant_id, company_id) REFERENCES crm_companies (tenant_id, id)
);
CREATE INDEX crm_contacts_company_idx ON crm_contacts (tenant_id, company_id) WHERE archived_at IS NULL;

-- ───────────────────────────── deals ─────────────────────────────
CREATE TABLE crm_deals (
  seq               bigint GENERATED ALWAYS AS IDENTITY,
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES organizations(id),
  name              text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
  company_id        uuid NOT NULL,
  contact_id        uuid,
  pipeline_key      text NOT NULL,
  stage             text NOT NULL,
  amount_minor      bigint CHECK (amount_minor BETWEEN 0 AND 1000000000000000),
  currency          text CHECK (currency ~ '^[A-Z]{3}$'),
  expected_close    date,
  owner_sub         text NOT NULL DEFAULT '' CHECK (char_length(owner_sub) <= 200),
  fields            jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(fields) = 'object'),
  stage_changed_at  timestamptz NOT NULL DEFAULT now(),
  version           bigint NOT NULL DEFAULT 1,
  idempotency_key   text NOT NULL,
  request_hash      bytea NOT NULL,
  archived_at       timestamptz,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, idempotency_key),
  UNIQUE (tenant_id, seq),
  CHECK ((amount_minor IS NULL) = (currency IS NULL)),
  FOREIGN KEY (tenant_id, company_id) REFERENCES crm_companies (tenant_id, id),
  FOREIGN KEY (tenant_id, contact_id) REFERENCES crm_contacts (tenant_id, id)
);
CREATE INDEX crm_deals_company_idx ON crm_deals (tenant_id, company_id) WHERE archived_at IS NULL;
CREATE INDEX crm_deals_stage_idx   ON crm_deals (tenant_id, pipeline_key, stage) WHERE archived_at IS NULL;

CREATE TABLE crm_deal_stage_history (
  seq          bigint GENERATED ALWAYS AS IDENTITY,
  tenant_id    uuid NOT NULL REFERENCES organizations(id),
  deal_id      uuid NOT NULL,
  from_stage   text NOT NULL DEFAULT '',
  to_stage     text NOT NULL,
  changed_by   text NOT NULL DEFAULT '' CHECK (char_length(changed_by) <= 200),
  changed_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, seq),
  FOREIGN KEY (tenant_id, deal_id) REFERENCES crm_deals (tenant_id, id)
);
CREATE INDEX crm_deal_stage_history_deal_idx ON crm_deal_stage_history (tenant_id, deal_id, seq);
CREATE TRIGGER crm_deal_stage_history_append_only BEFORE UPDATE OR DELETE ON crm_deal_stage_history
  FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ───────────────────────────── activities ─────────────────────────────
CREATE TABLE crm_activities (
  seq              bigint GENERATED ALWAYS AS IDENTITY,
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL REFERENCES organizations(id),
  kind             text NOT NULL CHECK (kind IN ('NOTE','CALL','EMAIL','MEETING','TASK')),
  subject          text NOT NULL CHECK (char_length(subject) BETWEEN 1 AND 200),
  body             text NOT NULL DEFAULT '' CHECK (char_length(body) <= 20000),
  due_at           timestamptz,
  completed_at     timestamptz,
  completed_by     text NOT NULL DEFAULT '' CHECK (char_length(completed_by) <= 200),
  company_id       uuid,
  contact_id       uuid,
  deal_id          uuid,
  owner_sub        text NOT NULL DEFAULT '' CHECK (char_length(owner_sub) <= 200),
  created_by       text NOT NULL DEFAULT '' CHECK (char_length(created_by) <= 200),
  idempotency_key  text NOT NULL,
  request_hash     bytea NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, idempotency_key),
  UNIQUE (tenant_id, seq),
  CHECK (company_id IS NOT NULL OR contact_id IS NOT NULL OR deal_id IS NOT NULL),
  CHECK (due_at IS NULL OR kind = 'TASK'),
  CHECK (completed_at IS NULL OR kind = 'TASK'),
  FOREIGN KEY (tenant_id, company_id) REFERENCES crm_companies (tenant_id, id),
  FOREIGN KEY (tenant_id, contact_id) REFERENCES crm_contacts (tenant_id, id),
  FOREIGN KEY (tenant_id, deal_id)    REFERENCES crm_deals (tenant_id, id)
);
CREATE INDEX crm_activities_deal_idx    ON crm_activities (tenant_id, deal_id)    WHERE deal_id IS NOT NULL;
CREATE INDEX crm_activities_company_idx ON crm_activities (tenant_id, company_id) WHERE company_id IS NOT NULL;
CREATE INDEX crm_activities_open_idx    ON crm_activities (tenant_id, owner_sub)  WHERE kind = 'TASK' AND completed_at IS NULL;

-- ───────────────────────────── immutable links, touch ─────────────────────────────
-- Which company a deal/contact belongs to, and which pipeline a deal is in, never change (the interface offers no
-- way to; this makes the database refuse it too).
CREATE FUNCTION crm_forbid_identity_change() RETURNS trigger
LANGUAGE plpgsql AS
$$
BEGIN
  -- Separate branches: PL/pgSQL resolves every NEW.<column> in an expression, so one expression naming a column
  -- the other table lacks would fail even behind a TG_TABLE_NAME test.
  IF TG_TABLE_NAME = 'crm_deals' THEN
    IF NEW.company_id IS DISTINCT FROM OLD.company_id OR NEW.pipeline_key IS DISTINCT FROM OLD.pipeline_key THEN
      RAISE EXCEPTION 'crm_deals.company_id and pipeline_key are immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
  ELSIF TG_TABLE_NAME = 'crm_contacts' THEN
    IF NEW.company_id IS DISTINCT FROM OLD.company_id THEN
      RAISE EXCEPTION 'crm_contacts.company_id is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
  END IF;
  RETURN NEW;
END
$$;
CREATE TRIGGER crm_deals_identity    BEFORE UPDATE ON crm_deals    FOR EACH ROW EXECUTE FUNCTION crm_forbid_identity_change();
CREATE TRIGGER crm_contacts_identity BEFORE UPDATE ON crm_contacts FOR EACH ROW EXECUTE FUNCTION crm_forbid_identity_change();

CREATE TRIGGER crm_companies_touch BEFORE UPDATE ON crm_companies FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
CREATE TRIGGER crm_contacts_touch  BEFORE UPDATE ON crm_contacts  FOR EACH ROW EXECUTE FUNCTION touch_updated_at();
CREATE TRIGGER crm_deals_touch     BEFORE UPDATE ON crm_deals     FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- ───────────────────────────── row-level security (same policies as schema.sql) ─────────────────────────────
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['crm_companies','crm_contacts','crm_deals','crm_deal_stage_history','crm_activities'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
    EXECUTE format('CREATE POLICY tenant_select ON %I FOR SELECT USING (tenant_id = ANY (app_tenant_ids()))', t);
    EXECUTE format('CREATE POLICY tenant_insert ON %I FOR INSERT WITH CHECK (tenant_id = app_tenant_id())', t);
    EXECUTE format('CREATE POLICY tenant_update ON %I FOR UPDATE USING (tenant_id = app_tenant_id()) WITH CHECK (tenant_id = app_tenant_id())', t);
    EXECUTE format('CREATE POLICY tenant_delete ON %I FOR DELETE USING (tenant_id = app_tenant_id())', t);
    EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE ON %I FOR EACH ROW EXECUTE FUNCTION forbid_tenant_change()',
                   t || '_tenant_immutable', t);
  END LOOP;
END
$$;

-- ───────────────────────────── privileges ─────────────────────────────
-- No DELETE on anything; the stage history is insert-only.
GRANT SELECT, INSERT, UPDATE ON crm_companies, crm_contacts, crm_deals, crm_activities TO venture_app;
GRANT SELECT, INSERT         ON crm_deal_stage_history TO venture_app;

INSERT INTO schema_version (version) VALUES (2);
COMMIT;

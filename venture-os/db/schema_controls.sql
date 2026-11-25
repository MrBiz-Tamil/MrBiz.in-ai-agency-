-- Venture OS controls and connections (schema version 3). Applied as venture_owner after schema.sql and schema_crm.sql.
--
--   1. Tamper-evident audit log: every audit row carries the hash of the previous row of the same tenant, computed by a
--      trigger so no insert path can skip it; audit_verify() re-walks the chain.
--   2. Spend caps: per-transaction, per-hour and per-day ceilings on money leaving a business, enforced by the control
--      plane when payments and purchase orders are approved.
--   3. Connections and sealed secrets for third-party providers (Google, Zoom, Notion). Secrets are ENCRYPTED by the
--      application (internal/vault, AES-256-GCM bound to tenant and name) before they reach this table.
--   4. The MEETING approval kind: scheduling or cancelling a meeting that notifies other people is outward-facing.
--
-- A hash chain makes tampering DETECTABLE, not impossible. Anyone who can rewrite the table and recompute every later
-- hash (a superuser) can forge a consistent chain; truncating the newest rows leaves a valid shorter chain. The defence
-- is to anchor the chain head somewhere the database owner cannot write (audit_verify returns it): see ARCHITECTURE.md.

BEGIN;

-- ───────────────────────────── 1. audit hash chain ─────────────────────────────
ALTER TABLE audit_logs ADD COLUMN prev_hash bytea, ADD COLUMN hash bytea;

-- A field is length-prefixed so that ("ab","c") and ("a","bc") can never produce the same bytes.
CREATE FUNCTION audit_field(v text) RETURNS text
LANGUAGE sql IMMUTABLE AS $$ SELECT length(coalesce(v, ''))::text || ':' || coalesce(v, '') $$;

-- The canonical bytes of a row: everything that says what happened, who did it and when, plus the tenant and id.
CREATE FUNCTION audit_canonical(r audit_logs) RETURNS bytea
LANGUAGE sql IMMUTABLE AS $$
  SELECT convert_to(
    audit_field(r.tenant_id::text) || audit_field(r.id::text) ||
    audit_field(to_char(r.occurred_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US')) ||
    audit_field(r.actor_type) || audit_field(r.actor_id) || audit_field(r.action) ||
    audit_field(r.entity_type) || audit_field(r.entity_id) || audit_field(r.outcome) || audit_field(r.request_id) ||
    audit_field(r.data::text), 'UTF8')
$$;

CREATE FUNCTION audit_chain() RETURNS trigger
LANGUAGE plpgsql AS
$$
DECLARE prev bytea;
BEGIN
  -- One writer at a time per tenant, held until commit, so the chain is a line and not a tree.
  PERFORM pg_advisory_xact_lock(hashtextextended('venture-audit:' || NEW.tenant_id::text, 0));
  SELECT hash INTO prev FROM audit_logs WHERE tenant_id = NEW.tenant_id AND hash IS NOT NULL ORDER BY id DESC LIMIT 1;
  NEW.prev_hash := prev;
  NEW.hash := sha256(audit_canonical(NEW) || coalesce(prev, ''::bytea));
  RETURN NEW;
END
$$;
CREATE TRIGGER audit_logs_chain BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION audit_chain();

-- Walks a tenant's chain. first_bad_id is NULL when it is intact; otherwise the first row whose stored hash or link to
-- its predecessor does not match what the data says. head is the newest hash: publish it somewhere the database owner
-- cannot write to detect truncation of recent rows.
CREATE FUNCTION audit_verify(p_tenant uuid)
RETURNS TABLE (first_bad_id bigint, rows_checked bigint, head bytea)
LANGUAGE plpgsql STABLE AS
$$
DECLARE
  r audit_logs;
  expected_prev bytea := NULL;
  checked bigint := 0;
  bad bigint := NULL;
BEGIN
  FOR r IN SELECT * FROM audit_logs WHERE tenant_id = p_tenant AND hash IS NOT NULL ORDER BY id LOOP
    checked := checked + 1;
    IF r.prev_hash IS DISTINCT FROM expected_prev
       OR r.hash IS DISTINCT FROM sha256(audit_canonical(r) || coalesce(r.prev_hash, ''::bytea)) THEN
      bad := r.id;
      EXIT;
    END IF;
    expected_prev := r.hash;
  END LOOP;
  RETURN QUERY SELECT bad, checked, expected_prev;
END
$$;

-- ───────────────────────────── 2. spend caps ─────────────────────────────
CREATE TABLE spend_caps (
  tenant_id            uuid NOT NULL REFERENCES organizations(id),
  currency             char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  -- NULL means "no ceiling on this dimension". All amounts are minor units.
  per_transaction_minor bigint CHECK (per_transaction_minor >= 0),
  per_hour_minor        bigint CHECK (per_hour_minor >= 0),
  per_day_minor         bigint CHECK (per_day_minor >= 0),
  updated_by           text NOT NULL,
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, currency),
  CHECK (per_hour_minor IS NULL OR per_day_minor IS NULL OR per_hour_minor <= per_day_minor),
  CHECK (per_transaction_minor IS NULL OR per_hour_minor IS NULL OR per_transaction_minor <= per_hour_minor)
);
CREATE TRIGGER spend_caps_touch BEFORE UPDATE ON spend_caps FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- ───────────────────────────── 3. connections and sealed secrets ─────────────────────────────
CREATE TABLE tenant_secrets (
  tenant_id    uuid NOT NULL REFERENCES organizations(id),
  name         text NOT NULL CHECK (name ~ '^[a-z][a-z0-9_.-]{0,62}$'),
  key_version  integer NOT NULL CHECK (key_version > 0),
  nonce        bytea NOT NULL CHECK (octet_length(nonce) = 12),
  ciphertext   bytea NOT NULL CHECK (octet_length(ciphertext) BETWEEN 17 AND 70000),
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, name)
);
CREATE TRIGGER tenant_secrets_touch BEFORE UPDATE ON tenant_secrets FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

CREATE TABLE tenant_connections (
  tenant_id     uuid NOT NULL REFERENCES organizations(id),
  provider      text NOT NULL CHECK (provider IN ('google','zoom','notion')),
  status        text NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE','DISABLED')),
  -- Non-secret settings only (calendar id, database id, user id). Credentials live in tenant_secrets.
  config        jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(config) = 'object'),
  secret_name   text NOT NULL,
  connected_by  text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, provider),
  FOREIGN KEY (tenant_id, secret_name) REFERENCES tenant_secrets (tenant_id, name)
);
CREATE TRIGGER tenant_connections_touch BEFORE UPDATE ON tenant_connections FOR EACH ROW EXECUTE FUNCTION touch_updated_at();

-- ───────────────────────────── 4. the MEETING approval kind ─────────────────────────────
ALTER TABLE approval_queue DROP CONSTRAINT approval_queue_kind_check;
ALTER TABLE approval_queue ADD CONSTRAINT approval_queue_kind_check CHECK (kind IN
  ('QUOTE','INVOICE','PURCHASE_ORDER','PAYMENT','CAMPAIGN','AGENT_SKILL','BLUEPRINT','MEETING','OTHER'));

-- ───────────────────────────── row-level security and privileges ─────────────────────────────
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['spend_caps','tenant_secrets','tenant_connections'] LOOP
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

GRANT SELECT, INSERT, UPDATE, DELETE ON spend_caps TO venture_app; -- removing a cap is a founder action, audited by the control plane
GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_secrets, tenant_connections TO venture_app;
GRANT EXECUTE ON FUNCTION audit_verify(uuid), audit_canonical(audit_logs), audit_field(text) TO venture_app;

INSERT INTO schema_version (version) VALUES (3);
COMMIT;

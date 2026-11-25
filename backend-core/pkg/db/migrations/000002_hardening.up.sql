-- Migration: 000002_hardening.up.sql
-- * Makes the Decision X-Ray audit log genuinely append-only
-- * Adds the tables the GraphQL schema already exposes (goals, milestones, policy rules)
-- * Adds users / sessions for real authentication
-- * Keeps updated_at honest

-- ---------------------------------------------------------------------------
-- 1. Immutable audit log
-- ---------------------------------------------------------------------------
ALTER TABLE audit_xray_logs DROP CONSTRAINT IF EXISTS audit_xray_logs_ticket_id_fkey;
ALTER TABLE audit_xray_logs DROP CONSTRAINT IF EXISTS audit_xray_logs_workspace_id_fkey;
ALTER TABLE audit_xray_logs
    ADD CONSTRAINT audit_xray_logs_ticket_id_fkey
    FOREIGN KEY (ticket_id) REFERENCES execution_tickets(id) ON DELETE RESTRICT;
ALTER TABLE audit_xray_logs
    ADD CONSTRAINT audit_xray_logs_workspace_id_fkey
    FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE RESTRICT;

CREATE OR REPLACE FUNCTION audit_xray_logs_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_xray_logs is append-only (% not allowed)', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_audit_xray_logs_no_update ON audit_xray_logs;
CREATE TRIGGER trg_audit_xray_logs_no_update
    BEFORE UPDATE OR DELETE ON audit_xray_logs
    FOR EACH ROW EXECUTE FUNCTION audit_xray_logs_immutable();

DROP TRIGGER IF EXISTS trg_audit_xray_logs_no_truncate ON audit_xray_logs;
CREATE TRIGGER trg_audit_xray_logs_no_truncate
    BEFORE TRUNCATE ON audit_xray_logs
    FOR EACH STATEMENT EXECUTE FUNCTION audit_xray_logs_immutable();

CREATE INDEX IF NOT EXISTS idx_audit_logs_workspace_created ON audit_xray_logs(workspace_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- 2. Money precision: audit costs have 6 decimals, so the running agent total must too
-- ---------------------------------------------------------------------------
ALTER TABLE agent_definitions ALTER COLUMN spent_today_usd TYPE NUMERIC(12, 6);
ALTER TABLE agent_definitions ADD CONSTRAINT chk_agent_budget_nonneg CHECK (daily_budget_usd >= 0 AND spent_today_usd >= 0);
ALTER TABLE workspaces ADD CONSTRAINT chk_workspace_limit_nonneg CHECK (daily_spend_limit_usd >= 0);

-- ---------------------------------------------------------------------------
-- 3. Goals, milestones and policy rules
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS business_goals (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    title VARCHAR(255) NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    target_metric VARCHAR(128) NOT NULL DEFAULT '',
    target_value NUMERIC(18, 4) NOT NULL DEFAULT 0,
    current_value NUMERIC(18, 4) NOT NULL DEFAULT 0,
    projected_margin_pct NUMERIC(7, 4) NOT NULL DEFAULT 0,
    deadline TIMESTAMPTZ NOT NULL,
    constraints JSONB NOT NULL DEFAULT '{}'::jsonb,
    status VARCHAR(32) NOT NULL DEFAULT 'DRAFT'
        CHECK (status IN ('DRAFT', 'ACTIVE', 'COMPLETED', 'CANCELLED')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_goals_workspace_status ON business_goals(workspace_id, status);

CREATE TABLE IF NOT EXISTS goal_milestones (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    goal_id UUID NOT NULL REFERENCES business_goals(id) ON DELETE CASCADE,
    step_order INT NOT NULL,
    action VARCHAR(255) NOT NULL,
    assigned_agent_role VARCHAR(128) NOT NULL,
    cron_schedule VARCHAR(64),
    is_completed BOOLEAN NOT NULL DEFAULT FALSE,
    UNIQUE (goal_id, step_order)
);

ALTER TABLE execution_tickets
    ADD CONSTRAINT execution_tickets_goal_id_fkey
    FOREIGN KEY (goal_id) REFERENCES business_goals(id) ON DELETE SET NULL;

CREATE TABLE IF NOT EXISTS policy_rules (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    rule_key VARCHAR(128) NOT NULL,
    trigger_event VARCHAR(128) NOT NULL,
    raw_policy_text TEXT NOT NULL,
    compiled_ast JSONB NOT NULL,
    assigned_approval_role VARCHAR(128) NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (workspace_id, rule_key)
);

-- ---------------------------------------------------------------------------
-- 4. Users and sessions (the session cookie stores only a random token; we keep its hash)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    email VARCHAR(320) NOT NULL,
    display_name VARCHAR(255) NOT NULL DEFAULT '',
    role VARCHAR(32) NOT NULL DEFAULT 'operator'
        CHECK (role IN ('owner', 'admin', 'operator', 'auditor')),
    password_hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (workspace_id, email)
);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash BYTEA PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_sessions_expiry ON sessions(expires_at);

-- ---------------------------------------------------------------------------
-- 5. updated_at maintenance
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION touch_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = CURRENT_TIMESTAMP;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['workspaces', 'mcp_connections', 'canonical_entities',
                             'agent_definitions', 'execution_tickets', 'business_goals', 'policy_rules']
    LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS trg_%1$s_touch ON %1$s', t);
        EXECUTE format('CREATE TRIGGER trg_%1$s_touch BEFORE UPDATE ON %1$s
                        FOR EACH ROW EXECUTE FUNCTION touch_updated_at()', t);
    END LOOP;
END $$;

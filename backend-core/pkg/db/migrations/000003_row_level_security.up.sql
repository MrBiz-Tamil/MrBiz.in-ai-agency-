-- Migration: 000003_row_level_security.up.sql
-- Tenant isolation enforced by PostgreSQL itself.
--
-- The application sets two session variables each time it acquires a connection:
--   app.workspace_id  the workspace the request belongs to ('' when none)
--   app.bypass_rls    'on' only for trusted system operations (workspace creation, startup)
-- With no workspace set, tenant tables return no rows: it fails closed.
--
-- Connect as (or SET ROLE to) abos_app so the policies apply. Superusers always bypass RLS.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'abos_app') THEN
        CREATE ROLE abos_app NOLOGIN;
    END IF;
END $$;

GRANT USAGE ON SCHEMA public TO abos_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO abos_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO abos_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO abos_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO abos_app;

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['mcp_connections', 'canonical_entities', 'external_id_map', 'agent_definitions',
                             'execution_tickets', 'audit_xray_logs', 'business_goals', 'goal_milestones', 'policy_rules']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (workspace_id::text = current_setting('app.workspace_id', true)
                   OR current_setting('app.bypass_rls', true) = 'on')
            WITH CHECK (workspace_id::text = current_setting('app.workspace_id', true)
                   OR current_setting('app.bypass_rls', true) = 'on')$p$, t);
    END LOOP;
END $$;

-- workspaces: a tenant sees only its own row
ALTER TABLE workspaces ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspaces FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON workspaces;
CREATE POLICY tenant_isolation ON workspaces
    USING (id::text = current_setting('app.workspace_id', true)
           OR current_setting('app.bypass_rls', true) = 'on')
    WITH CHECK (id::text = current_setting('app.workspace_id', true)
           OR current_setting('app.bypass_rls', true) = 'on');

-- users and sessions are consulted before a tenant is known (login, session lookup), so they
-- stay outside RLS; every query on them is keyed by email or by the hash of a secret token.

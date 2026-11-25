-- Migration: 000003_row_level_security.down.sql
DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['workspaces', 'mcp_connections', 'canonical_entities', 'external_id_map', 'agent_definitions',
                             'execution_tickets', 'audit_xray_logs', 'business_goals', 'goal_milestones', 'policy_rules']
    LOOP
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', t);
        EXECUTE format('ALTER TABLE %I NO FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I DISABLE ROW LEVEL SECURITY', t);
    END LOOP;
END $$;
-- The abos_app role and its grants are intentionally left in place (they may be in use).

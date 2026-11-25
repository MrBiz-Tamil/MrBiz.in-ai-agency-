-- Migration: 000002_hardening.down.sql
DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['workspaces', 'mcp_connections', 'canonical_entities',
                             'agent_definitions', 'execution_tickets', 'business_goals', 'policy_rules']
    LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS trg_%1$s_touch ON %1$s', t);
    END LOOP;
END $$;
DROP FUNCTION IF EXISTS touch_updated_at();

DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS policy_rules;
ALTER TABLE execution_tickets DROP CONSTRAINT IF EXISTS execution_tickets_goal_id_fkey;
DROP TABLE IF EXISTS goal_milestones;
DROP TABLE IF EXISTS business_goals;

ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS chk_workspace_limit_nonneg;
ALTER TABLE agent_definitions DROP CONSTRAINT IF EXISTS chk_agent_budget_nonneg;
ALTER TABLE agent_definitions ALTER COLUMN spent_today_usd TYPE NUMERIC(10, 2);

DROP TRIGGER IF EXISTS trg_audit_xray_logs_no_truncate ON audit_xray_logs;
DROP TRIGGER IF EXISTS trg_audit_xray_logs_no_update ON audit_xray_logs;
DROP FUNCTION IF EXISTS audit_xray_logs_immutable();
DROP INDEX IF EXISTS idx_audit_logs_workspace_created;
ALTER TABLE audit_xray_logs DROP CONSTRAINT IF EXISTS audit_xray_logs_ticket_id_fkey;
ALTER TABLE audit_xray_logs DROP CONSTRAINT IF EXISTS audit_xray_logs_workspace_id_fkey;
ALTER TABLE audit_xray_logs
    ADD CONSTRAINT audit_xray_logs_ticket_id_fkey
    FOREIGN KEY (ticket_id) REFERENCES execution_tickets(id) ON DELETE CASCADE;
ALTER TABLE audit_xray_logs
    ADD CONSTRAINT audit_xray_logs_workspace_id_fkey
    FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE;

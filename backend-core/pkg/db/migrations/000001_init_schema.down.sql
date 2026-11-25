-- Migration: 000001_init_schema.down.sql
DROP TABLE IF EXISTS audit_xray_logs CASCADE;
DROP TABLE IF EXISTS execution_tickets CASCADE;
DROP TABLE IF EXISTS agent_definitions CASCADE;
DROP TABLE IF EXISTS external_id_map CASCADE;
DROP TABLE IF EXISTS canonical_entities CASCADE;
DROP TABLE IF EXISTS mcp_connections CASCADE;
DROP TABLE IF EXISTS workspaces CASCADE;

-- Migration: 000001_init_schema.up.sql
-- gen_random_uuid() is built into PostgreSQL 13+, so no extension is required.

-- Workspaces
CREATE TABLE IF NOT EXISTS workspaces (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(255) NOT NULL,
    default_currency VARCHAR(3) NOT NULL DEFAULT 'INR',
    tax_jurisdiction VARCHAR(64) NOT NULL DEFAULT 'GST_INDIA',
    daily_spend_limit_usd NUMERIC(10, 2) NOT NULL DEFAULT 100.00,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- MCP Connections Registry
CREATE TABLE IF NOT EXISTS mcp_connections (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    server_name VARCHAR(128) NOT NULL,
    transport_type VARCHAR(16) NOT NULL CHECK (transport_type IN ('http', 'stdio')),
    server_uri TEXT NOT NULL,
    encrypted_auth_ref TEXT NOT NULL,
    health_status VARCHAR(32) NOT NULL DEFAULT 'HEALTHY',
    allowed_scopes JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Canonical Entities Store
CREATE TABLE IF NOT EXISTS canonical_entities (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    entity_type VARCHAR(32) NOT NULL CHECK (entity_type IN ('SKU', 'ORDER', 'CUSTOMER', 'INVOICE', 'VENDOR')),
    canonical_payload JSONB NOT NULL,
    version INT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_canonical_entities_lookup ON canonical_entities(workspace_id, entity_type);

-- External System ID Mapping
CREATE TABLE IF NOT EXISTS external_id_map (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    canonical_id UUID NOT NULL REFERENCES canonical_entities(id) ON DELETE CASCADE,
    system_name VARCHAR(64) NOT NULL CHECK (system_name IN ('shopify', 'odoo', 'sap', 'zoho', 'bigcommerce', 'notion')),
    external_id VARCHAR(255) NOT NULL,
    last_synced_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (workspace_id, system_name, external_id)
);
CREATE INDEX IF NOT EXISTS idx_external_id_map_canonical ON external_id_map(canonical_id);

-- Agent Definitions & Governance
CREATE TABLE IF NOT EXISTS agent_definitions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    role_name VARCHAR(128) NOT NULL,
    model_tier VARCHAR(32) NOT NULL DEFAULT 'FLASH' CHECK (model_tier IN ('FLASH', 'FRONTIER')),
    heartbeat_cron VARCHAR(64),
    daily_budget_usd NUMERIC(10, 2) NOT NULL DEFAULT 10.00,
    spent_today_usd NUMERIC(10, 2) NOT NULL DEFAULT 0.00,
    status VARCHAR(32) NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'PAUSED', 'BUDGET_CAPPED')),
    playbook_ref TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Execution Tickets
CREATE TABLE IF NOT EXISTS execution_tickets (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    goal_id UUID,
    assigned_agent_id UUID REFERENCES agent_definitions(id) ON DELETE SET NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'QUEUED' 
        CHECK (status IN ('QUEUED', 'RUNNING', 'PENDING_APPROVAL', 'CONFLICT_HOLD', 'COMPLETED', 'FAILED')),
    idempotency_key VARCHAR(255) NOT NULL,
    priority INT NOT NULL DEFAULT 5,
    action_type VARCHAR(128) NOT NULL,
    payload JSONB NOT NULL,
    result JSONB,
    conflict_diff JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (workspace_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_tickets_status ON execution_tickets(workspace_id, status);

-- Audit X-Ray Logs
CREATE TABLE IF NOT EXISTS audit_xray_logs (
    id BIGSERIAL PRIMARY KEY,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    ticket_id UUID NOT NULL REFERENCES execution_tickets(id) ON DELETE CASCADE,
    step_number INT NOT NULL,
    tool_name VARCHAR(128),
    input_sanitized JSONB,
    output_sanitized JSONB,
    policy_decision VARCHAR(64) NOT NULL DEFAULT 'ALLOW',
    token_cost NUMERIC(10, 6) NOT NULL DEFAULT 0.000000,
    latency_ms INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_audit_logs_ticket ON audit_xray_logs(ticket_id);

-- name: GetAgent :one
SELECT * FROM agent_definitions
WHERE id = $1 AND workspace_id = $2
LIMIT 1;

-- name: ListAgents :many
SELECT * FROM agent_definitions
WHERE workspace_id = $1
ORDER BY role_name ASC;

-- name: CreateAgent :one
INSERT INTO agent_definitions (
    workspace_id, role_name, model_tier, heartbeat_cron, daily_budget_usd, playbook_ref
) VALUES (
    $1, $2, $3, $4, $5, $6
) RETURNING *;

-- name: UpdateAgentSpend :one
UPDATE agent_definitions
SET spent_today_usd = spent_today_usd + $3, updated_at = CURRENT_TIMESTAMP
WHERE id = $1 AND workspace_id = $2
RETURNING *;

-- name: UpdateAgentStatus :one
UPDATE agent_definitions
SET status = $3, updated_at = CURRENT_TIMESTAMP
WHERE id = $1 AND workspace_id = $2
RETURNING *;

-- name: GetWorkspace :one
SELECT * FROM workspaces
WHERE id = $1 LIMIT 1;

-- name: CreateWorkspace :one
INSERT INTO workspaces (
    name, default_currency, tax_jurisdiction, daily_spend_limit_usd
) VALUES (
    $1, $2, $3, $4
) RETURNING *;

-- name: UpdateWorkspaceSpendLimit :one
UPDATE workspaces
SET daily_spend_limit_usd = $2, updated_at = CURRENT_TIMESTAMP
WHERE id = $1
RETURNING *;

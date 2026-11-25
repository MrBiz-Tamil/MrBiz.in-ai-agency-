-- name: GetTicketById :one
SELECT * FROM execution_tickets
WHERE id = $1 LIMIT 1;

-- name: GetTicketByIdempotencyKey :one
SELECT * FROM execution_tickets
WHERE workspace_id = $1 AND idempotency_key = $2
LIMIT 1;

-- name: CreateTicket :one
INSERT INTO execution_tickets (
    workspace_id, goal_id, assigned_agent_id, status,
    idempotency_key, priority, action_type, payload
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8
) RETURNING *;

-- name: UpdateTicketStatus :one
UPDATE execution_tickets
SET status = $2, result = $3, conflict_diff = $4, updated_at = CURRENT_TIMESTAMP
WHERE id = $1
RETURNING *;

-- name: ListTicketsByStatus :many
SELECT * FROM execution_tickets
WHERE workspace_id = $1 AND status = $2
ORDER BY priority ASC, created_at DESC
LIMIT $3 OFFSET $4;

-- name: ListRecentTickets :many
SELECT * FROM execution_tickets
WHERE workspace_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

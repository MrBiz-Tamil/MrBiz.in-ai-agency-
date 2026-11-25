-- name: CreateAuditLog :one
INSERT INTO audit_xray_logs (
    workspace_id, ticket_id, step_number, tool_name,
    input_sanitized, output_sanitized, policy_decision,
    token_cost, latency_ms
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
) RETURNING *;

-- name: ListAuditLogsByTicket :many
SELECT * FROM audit_xray_logs
WHERE ticket_id = $1
ORDER BY step_number ASC;

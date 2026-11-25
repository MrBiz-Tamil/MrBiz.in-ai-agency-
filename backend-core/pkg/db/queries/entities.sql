-- name: GetCanonicalEntity :one
SELECT * FROM canonical_entities
WHERE id = $1 AND workspace_id = $2
LIMIT 1;

-- name: UpsertCanonicalEntity :one
INSERT INTO canonical_entities (
    workspace_id, entity_type, canonical_payload, version
) VALUES (
    $1, $2, $3, $4
)
ON CONFLICT (id) DO UPDATE
SET canonical_payload = EXCLUDED.canonical_payload,
    version = canonical_entities.version + 1,
    updated_at = CURRENT_TIMESTAMP
RETURNING *;

-- name: GetExternalMapping :one
SELECT * FROM external_id_map
WHERE workspace_id = $1 AND system_name = $2 AND external_id = $3
LIMIT 1;

-- name: SetExternalMapping :one
INSERT INTO external_id_map (
    workspace_id, canonical_id, system_name, external_id
) VALUES (
    $1, $2, $3, $4
)
ON CONFLICT (workspace_id, system_name, external_id) DO UPDATE
SET canonical_id = EXCLUDED.canonical_id,
    last_synced_at = CURRENT_TIMESTAMP
RETURNING *;

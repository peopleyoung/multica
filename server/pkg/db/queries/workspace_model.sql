-- name: ListWorkspaceModels :many
SELECT * FROM workspace_model WHERE workspace_id = $1 ORDER BY is_default DESC, created_at, id;

-- name: GetWorkspaceModel :one
SELECT * FROM workspace_model WHERE workspace_id = $1 AND id = $2;

-- name: GetDefaultWorkspaceModel :one
SELECT * FROM workspace_model WHERE workspace_id = $1 AND is_default AND enabled;

-- name: PutWorkspaceModel :one
INSERT INTO workspace_model (id, workspace_id, name, base_url, upstream_model, api_key_encrypted, capabilities, is_default, enabled)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name, base_url=EXCLUDED.base_url,
upstream_model=EXCLUDED.upstream_model, api_key_encrypted=EXCLUDED.api_key_encrypted,
capabilities=EXCLUDED.capabilities, is_default=EXCLUDED.is_default, enabled=EXCLUDED.enabled, updated_at=now()
WHERE workspace_model.workspace_id=EXCLUDED.workspace_id RETURNING *;

-- name: ClearDefaultWorkspaceModel :exec
UPDATE workspace_model SET is_default=false, updated_at=now() WHERE workspace_id=$1 AND is_default;

-- name: DeleteWorkspaceModel :execrows
DELETE FROM workspace_model WHERE workspace_id=$1 AND id=$2;

-- name: WorkspaceModelAssigned :one
SELECT EXISTS (SELECT 1 FROM agent WHERE workspace_id=$1 AND model=$2);

-- name: PinTaskNativeModel :one
UPDATE agent_task_queue SET native_model_config=COALESCE(native_model_config, sqlc.arg(config)::jsonb)
WHERE id=sqlc.arg(id) AND agent_id=sqlc.arg(agent_id)
RETURNING native_model_config;

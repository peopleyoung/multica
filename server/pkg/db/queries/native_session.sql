-- name: AcquireNativeSession :one
-- A claim is stable across duplicate acquire calls. Another executor must
-- wait for lease expiry and supplies its own randomly generated fencing token.
-- Lock the workspace before the run so teardown cannot miss a late insert.
-- FOR SHARE rechecks the running claim after concurrent cancellation; the
-- run's cancellation/deletion must finish before scope history is pruned.
WITH active_workspace AS MATERIALIZED (
    SELECT id FROM workspace WHERE id = $1 FOR KEY SHARE
), active_claim AS MATERIALIZED (
    SELECT t.id FROM agent_task_queue t
    JOIN active_workspace w ON true
    WHERE t.id = $7 AND t.agent_id = $2 AND t.status = 'running'
      AND t.dispatched_at = $8
    FOR SHARE OF t
)
INSERT INTO native_session (
    workspace_id, agent_id, scope_type, scope_id, daemon_id, work_dir,
    active_task_id, active_dispatch, lease_token, lease_until, generation_task_id
) SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, now() + interval '45 seconds', $7
FROM active_claim
WHERE true
ON CONFLICT (workspace_id, agent_id, scope_type, scope_id) DO UPDATE SET
    active_task_id = EXCLUDED.active_task_id,
    active_dispatch = EXCLUDED.active_dispatch,
    lease_token = EXCLUDED.lease_token,
    lease_until = now() + interval '45 seconds',
    checkpoint = CASE WHEN sqlc.arg(fresh_start)::boolean
        AND native_session.generation_task_id IS DISTINCT FROM EXCLUDED.generation_task_id
        THEN '{"messages":[],"tools":[]}'::jsonb ELSE native_session.checkpoint END,
    write_outcomes = CASE WHEN sqlc.arg(fresh_start)::boolean
        AND native_session.generation_task_id IS DISTINCT FROM EXCLUDED.generation_task_id
        THEN '{}'::jsonb ELSE native_session.write_outcomes END,
    version = CASE WHEN sqlc.arg(fresh_start)::boolean
        AND native_session.generation_task_id IS DISTINCT FROM EXCLUDED.generation_task_id
        THEN native_session.version + 1 ELSE native_session.version END,
    generation_task_id = CASE WHEN sqlc.arg(fresh_start)::boolean
        THEN EXCLUDED.generation_task_id ELSE native_session.generation_task_id END,
    work_dir = CASE WHEN sqlc.arg(fresh_start)::boolean
        AND native_session.generation_task_id IS DISTINCT FROM EXCLUDED.generation_task_id
        THEN EXCLUDED.work_dir ELSE native_session.work_dir END,
    updated_at = now()
WHERE native_session.daemon_id = EXCLUDED.daemon_id
  AND (native_session.work_dir = EXCLUDED.work_dir
       OR (sqlc.arg(fresh_start)::boolean
           AND native_session.generation_task_id IS DISTINCT FROM EXCLUDED.generation_task_id))
  AND (NOT sqlc.arg(fresh_start)::boolean
       OR native_session.generation_task_id = EXCLUDED.generation_task_id
       OR jsonb_array_length(COALESCE(native_session.checkpoint->'tools', '[]'::jsonb)) = 0)
  AND (native_session.lease_until IS NULL OR native_session.lease_until <= now()
       OR (native_session.active_task_id = EXCLUDED.active_task_id
           AND native_session.active_dispatch = EXCLUDED.active_dispatch
           AND native_session.lease_token = EXCLUDED.lease_token))
RETURNING *;

-- name: SaveNativeCheckpoint :one
-- The complete model/tool history is one atomic JSON document, rather than
-- dual-written display rows. Version and lease fencing guard every mutation.
UPDATE native_session AS s SET checkpoint = sqlc.arg(checkpoint), version = s.version + 1,
    lease_until = now() + interval '45 seconds', updated_at = now()
WHERE s.id = sqlc.arg(id) AND s.workspace_id = sqlc.arg(workspace_id)
  AND s.agent_id = sqlc.arg(agent_id) AND s.active_task_id = sqlc.arg(task_id)
  AND s.active_dispatch = sqlc.arg(dispatched_at) AND s.lease_token = sqlc.arg(lease_token)
  AND s.version = sqlc.arg(expected_version) AND s.lease_until > now()
  AND EXISTS (SELECT 1 FROM agent_task_queue t
      WHERE t.id = sqlc.arg(task_id) AND t.agent_id = sqlc.arg(agent_id)
        AND t.status = 'running' AND t.dispatched_at = sqlc.arg(dispatched_at))
RETURNING *;

-- name: GetNativeSessionForClaim :one
SELECT * FROM native_session
WHERE id = sqlc.arg(id) AND workspace_id = sqlc.arg(workspace_id)
  AND agent_id = sqlc.arg(agent_id) AND active_task_id = sqlc.arg(task_id)
  AND active_dispatch = sqlc.arg(dispatched_at) AND lease_token = sqlc.arg(lease_token)
  AND version = sqlc.arg(expected_version) AND lease_until > now();

-- name: RenewNativeSession :one
UPDATE native_session AS s SET lease_until = now() + interval '45 seconds', updated_at = now()
WHERE s.id = sqlc.arg(id) AND s.workspace_id = sqlc.arg(workspace_id)
  AND s.agent_id = sqlc.arg(agent_id) AND s.active_task_id = sqlc.arg(task_id)
  AND s.active_dispatch = sqlc.arg(dispatched_at) AND s.lease_token = sqlc.arg(lease_token)
  AND s.lease_until > now()
  AND EXISTS (SELECT 1 FROM agent_task_queue t
      WHERE t.id = sqlc.arg(task_id) AND t.agent_id = sqlc.arg(agent_id)
        AND t.status = 'running' AND t.dispatched_at = sqlc.arg(dispatched_at))
RETURNING *;

-- name: ReleaseNativeSession :execrows
UPDATE native_session SET lease_until = NULL, active_task_id = NULL,
    active_dispatch = NULL, lease_token = NULL, updated_at = now()
WHERE id = sqlc.arg(id) AND workspace_id = sqlc.arg(workspace_id)
  AND agent_id = sqlc.arg(agent_id) AND active_task_id = sqlc.arg(task_id)
  AND active_dispatch = sqlc.arg(dispatched_at) AND lease_token = sqlc.arg(lease_token);

-- name: DeleteNativeSessionsByScope :exec
DELETE FROM native_session WHERE workspace_id = $1 AND scope_type = $2 AND scope_id = $3;

-- name: DeleteNativeSessionsByAgent :exec
DELETE FROM native_session WHERE workspace_id = $1 AND agent_id = $2;

-- name: DeleteNativeSessionsByWorkspace :exec
DELETE FROM native_session WHERE workspace_id = $1;

-- name: GetNativeSessionForWrite :one
SELECT * FROM native_session
WHERE id = sqlc.arg(id) AND workspace_id = sqlc.arg(workspace_id)
  AND agent_id = sqlc.arg(agent_id) AND active_task_id = sqlc.arg(task_id)
  AND lease_token = sqlc.arg(lease_token) AND version = sqlc.arg(expected_version)
  AND lease_until > now();

-- name: ReserveNativeWrite :execrows
-- A durable intent is never automatically repeated, even if the API process
-- exits between the business commit and recording the HTTP result.
UPDATE native_session AS s
SET write_outcomes = s.write_outcomes || jsonb_build_object(sqlc.arg(operation_key)::text, sqlc.arg(intent)::jsonb)
WHERE s.id = sqlc.arg(id) AND s.workspace_id = sqlc.arg(workspace_id)
  AND s.agent_id = sqlc.arg(agent_id) AND s.active_task_id = sqlc.arg(task_id)
  AND s.lease_token = sqlc.arg(lease_token) AND s.version = sqlc.arg(expected_version)
  AND s.lease_until > now()
  AND NOT (s.write_outcomes ? sqlc.arg(operation_key)::text)
  AND octet_length(s.write_outcomes::text) < 4194304
  AND EXISTS (SELECT 1 FROM agent_task_queue t WHERE t.id = s.active_task_id
      AND t.agent_id = s.agent_id AND t.status = 'running' AND t.dispatched_at = s.active_dispatch);

-- name: FinishNativeWrite :execrows
-- Outcome recording remains valid after cancellation; it cannot authorize a
-- new write or modify the model checkpoint/version.
UPDATE native_session AS s
SET write_outcomes = jsonb_set(s.write_outcomes, ARRAY[sqlc.arg(operation_key)::text], sqlc.arg(outcome)::jsonb)
WHERE s.id = sqlc.arg(id) AND s.workspace_id = sqlc.arg(workspace_id)
  AND s.agent_id = sqlc.arg(agent_id)
  AND s.write_outcomes -> sqlc.arg(operation_key)::text ->> 'digest' = sqlc.arg(digest)::text
  AND s.write_outcomes -> sqlc.arg(operation_key)::text ->> 'state' = 'started';

-- name: LockNativeJobSourceTask :one
-- Completed source runs remain the provenance of independent jobs. Serialize
-- their deterministic feedback receipts with deletion and concurrent delivery.
SELECT * FROM agent_task_queue WHERE id = $1 FOR UPDATE;

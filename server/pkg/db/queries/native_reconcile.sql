-- name: GetNativeSessionForOperator :one
SELECT s.* FROM native_session s
WHERE s.id = sqlc.arg(id)
  AND EXISTS (SELECT 1 FROM member m
      WHERE m.workspace_id = s.workspace_id AND m.user_id = sqlc.arg(operator_id)
        AND m.role IN ('owner', 'admin'));

-- name: ReconcileInactiveNativeCheckpoint :one
-- An operator can settle a verified shell outcome only while execution is idle.
-- The ordinary checkpoint path still rejects unknown-to-terminal transitions.
UPDATE native_session s
SET checkpoint = sqlc.arg(checkpoint), version = s.version + 1, updated_at = now()
WHERE s.id = sqlc.arg(id) AND s.version = sqlc.arg(expected_version)
  AND (s.lease_until IS NULL OR s.lease_until <= now())
  AND EXISTS (SELECT 1 FROM member m
      WHERE m.workspace_id = s.workspace_id AND m.user_id = sqlc.arg(operator_id)
        AND m.role IN ('owner', 'admin'))
  AND NOT EXISTS (SELECT 1 FROM agent_task_queue t
      WHERE t.agent_id = s.agent_id AND t.status IN ('running', 'dispatched')
        AND ((s.scope_type = 'issue' AND t.issue_id = s.scope_id)
          OR (s.scope_type = 'chat' AND t.chat_session_id = s.scope_id)
          OR (s.scope_type = 'run' AND t.id = s.scope_id)))
RETURNING s.*;

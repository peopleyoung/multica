-- name: LockModelAdmission :exec
-- Shared by every API replica and purpose. No connection is held during inference.
SELECT pg_advisory_xact_lock(78164321);

-- name: LockModelCallWorkspace :one
SELECT id FROM workspace WHERE id=$1 FOR KEY SHARE;

-- name: GetModelAdmissionTotals :one
SELECT
    count(*) FILTER (WHERE outcome='active' AND lease_until>now())::bigint AS global_active,
    count(*) FILTER (WHERE workspace_id=sqlc.arg(workspace_id) AND outcome='active' AND lease_until>now())::bigint AS workspace_active,
    count(*) FILTER (WHERE scope_key=sqlc.arg(scope_key) AND workspace_id=sqlc.arg(workspace_id) AND outcome='active' AND lease_until>now())::bigint AS scope_active,
    count(*) FILTER (WHERE acquired_at>now()-interval '1 minute')::bigint AS global_requests,
    count(*) FILTER (WHERE workspace_id=sqlc.arg(workspace_id) AND acquired_at>now()-interval '1 minute')::bigint AS workspace_requests,
    coalesce(sum(charged_tokens) FILTER (WHERE acquired_at>now()-interval '1 minute'), 0)::bigint AS global_tokens,
    coalesce(sum(charged_tokens) FILTER (WHERE workspace_id=sqlc.arg(workspace_id) AND acquired_at>now()-interval '1 minute'), 0)::bigint AS workspace_tokens
FROM model_call
WHERE acquired_at>now()-interval '1 minute' OR (outcome='active' AND lease_until>now());

-- name: ReserveModelCall :one
INSERT INTO model_call (workspace_id, task_id, session_id, purpose, scope_key, model, retry_attempt, reserved_tokens, charged_tokens)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8) RETURNING id;

-- name: FinishModelCall :execrows
UPDATE model_call SET finished_at=now(), outcome=sqlc.arg(outcome),
    input_tokens=sqlc.narg(input_tokens), output_tokens=sqlc.narg(output_tokens), cached_tokens=sqlc.narg(cached_tokens),
    charged_tokens=CASE WHEN sqlc.narg(input_tokens)::bigint IS NULL THEN reserved_tokens
                       ELSE GREATEST(1,sqlc.narg(input_tokens)::bigint+sqlc.narg(output_tokens)::bigint) END,
    duration_ms=sqlc.arg(duration_ms), http_status=sqlc.arg(http_status)
WHERE id=sqlc.arg(id) AND workspace_id=sqlc.arg(workspace_id) AND outcome='active';

-- name: PruneModelCalls :exec
DELETE FROM model_call WHERE id IN (
    SELECT id FROM model_call WHERE acquired_at<now()-interval '7 days' AND lease_until<now()
    ORDER BY acquired_at LIMIT 128
);

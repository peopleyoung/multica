-- No foreign keys: application services own authorization and cleanup.
-- All indexes, including identity uniqueness, are built in separate files.
CREATE TABLE IF NOT EXISTS native_session (
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    agent_id UUID NOT NULL,
    scope_type TEXT NOT NULL CHECK (scope_type IN ('chat', 'issue', 'run')),
    scope_id UUID NOT NULL,
    daemon_id TEXT NOT NULL,
    work_dir TEXT NOT NULL,
    checkpoint JSONB NOT NULL DEFAULT '{"messages":[],"tools":[]}'::jsonb,
    version BIGINT NOT NULL DEFAULT 0,
    active_task_id UUID,
    active_dispatch TIMESTAMPTZ,
    lease_token UUID,
    lease_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

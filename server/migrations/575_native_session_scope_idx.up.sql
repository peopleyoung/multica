CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS native_session_scope_idx ON native_session (workspace_id, agent_id, scope_type, scope_id);

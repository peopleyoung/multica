CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS workspace_model_default_idx ON workspace_model (workspace_id) WHERE is_default;

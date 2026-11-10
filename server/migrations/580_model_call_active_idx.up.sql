CREATE INDEX CONCURRENTLY IF NOT EXISTS model_call_active_idx ON model_call (lease_until) WHERE outcome = 'active';

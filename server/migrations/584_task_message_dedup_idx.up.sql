CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS task_message_dedup_idx ON task_message (task_id, dedup_key) WHERE dedup_key IS NOT NULL;

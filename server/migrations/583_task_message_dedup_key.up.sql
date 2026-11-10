ALTER TABLE task_message ADD COLUMN IF NOT EXISTS dedup_key text;

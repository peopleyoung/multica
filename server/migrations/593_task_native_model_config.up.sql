-- Pin encrypted configuration at first model use so edits affect later runs.
ALTER TABLE agent_task_queue ADD COLUMN IF NOT EXISTS native_model_config JSONB;

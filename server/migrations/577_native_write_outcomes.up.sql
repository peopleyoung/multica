-- The session owns write outcomes, so existing scope/agent/workspace deletion
-- removes them together with the corresponding model and tool history.
ALTER TABLE native_session ADD COLUMN IF NOT EXISTS write_outcomes JSONB NOT NULL DEFAULT '{}'::jsonb;

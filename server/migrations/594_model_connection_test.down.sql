-- Refuse rollback while diagnostic audit rows exist. Archive those records
-- explicitly before retrying; rollback must never silently delete usage data.
ALTER TABLE IF EXISTS model_call
    DROP CONSTRAINT IF EXISTS model_call_purpose_check,
    ADD CONSTRAINT model_call_purpose_check CHECK (purpose IN ('execution', 'summary', 'title', 'quick_actions'));

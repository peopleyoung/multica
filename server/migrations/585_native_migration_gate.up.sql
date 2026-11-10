ALTER TABLE workspace ADD COLUMN IF NOT EXISTS native_migration_state TEXT NOT NULL DEFAULT 'none'
    CHECK (native_migration_state IN ('none', 'draining', 'native'));
ALTER TABLE workspace ADD COLUMN IF NOT EXISTS native_migration_receipt JSONB NOT NULL DEFAULT '{}';

-- Serialize dispatch with the durable maintenance gate, including requests
-- from an older API. Already-owned work can start and finish while draining.
CREATE OR REPLACE FUNCTION fence_native_migration_dispatch() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    migration_state TEXT;
    task_workspace UUID;
    task_provider TEXT;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.status NOT IN ('dispatched', 'running', 'waiting_local_directory') OR
           (OLD.status IN ('dispatched', 'running', 'waiting_local_directory') AND
            NEW.runtime_id IS NOT DISTINCT FROM OLD.runtime_id AND
            NEW.dispatched_at IS NOT DISTINCT FROM OLD.dispatched_at) THEN
            RETURN NEW;
        END IF;
    ELSIF NEW.status NOT IN ('dispatched', 'running', 'waiting_local_directory') THEN
        RETURN NEW;
    END IF;
    SELECT workspace_id INTO task_workspace FROM agent WHERE id = NEW.agent_id;
    SELECT native_migration_state INTO migration_state FROM workspace WHERE id = task_workspace FOR SHARE;
    IF migration_state = 'draining' THEN
        RETURN NULL;
    END IF;
    IF migration_state = 'native' THEN
        SELECT provider INTO task_provider FROM agent_runtime WHERE id = NEW.runtime_id AND workspace_id = task_workspace;
        IF task_provider IS DISTINCT FROM 'native' THEN
            RETURN NULL;
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_native_migration_dispatch ON agent_task_queue;
CREATE TRIGGER trg_native_migration_dispatch BEFORE INSERT OR UPDATE OF status, runtime_id, dispatched_at
    ON agent_task_queue FOR EACH ROW EXECUTE FUNCTION fence_native_migration_dispatch();

DROP TRIGGER IF EXISTS trg_native_migration_dispatch ON agent_task_queue;
DROP FUNCTION IF EXISTS fence_native_migration_dispatch();
ALTER TABLE workspace DROP COLUMN IF EXISTS native_migration_receipt;
ALTER TABLE workspace DROP COLUMN IF EXISTS native_migration_state;

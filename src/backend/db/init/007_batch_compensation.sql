ALTER TABLE migration_jobs
    ADD COLUMN IF NOT EXISTS cancelled_execution_id BIGINT REFERENCES migration_history(execution_id);

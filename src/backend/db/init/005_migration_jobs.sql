CREATE TABLE IF NOT EXISTS migration_jobs (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job_key TEXT NOT NULL UNIQUE DEFAULT gen_random_uuid()::text,
    database_id BIGINT NOT NULL,
    version BIGINT NOT NULL,
    actor BIGINT NOT NULL REFERENCES users(id),
    direction TEXT NOT NULL CHECK (direction IN ('up', 'rollback', 'forward', 'cancel')),
    mode TEXT NOT NULL CHECK (mode IN ('normal', 'batch')),
    status TEXT NOT NULL,
    sql_text TEXT NOT NULL,
    sql_down TEXT NOT NULL,
    batch_table TEXT NOT NULL,
    batch_size INTEGER NOT NULL,
    source_job BIGINT REFERENCES migration_jobs(id) ON DELETE SET NULL,
    execution_id BIGINT REFERENCES migration_history(execution_id),
    batches_done BIGINT NOT NULL DEFAULT 0,
    rows_done BIGINT NOT NULL DEFAULT 0,
    attempts INTEGER NOT NULL DEFAULT 0,
    generation INTEGER NOT NULL DEFAULT 1,
    blocks_database BOOLEAN NOT NULL DEFAULT FALSE,
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ,
    traceparent TEXT NOT NULL DEFAULT '',
    tracestate TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (database_id, version) REFERENCES migrations(database_id, version) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS migration_jobs_dispatch_idx
    ON migration_jobs (available_at, id) WHERE status = 'pending' AND published_at IS NULL;
CREATE INDEX IF NOT EXISTS migration_jobs_database_idx ON migration_jobs (database_id, id);
CREATE UNIQUE INDEX IF NOT EXISTS migration_jobs_one_blocker_idx
    ON migration_jobs (database_id) WHERE blocks_database;

CREATE UNIQUE INDEX IF NOT EXISTS migration_jobs_one_request_idx
    ON migration_jobs (database_id,version,direction) WHERE status IN ('pending_review','pending','running','partial','uncertain','partially_reverted');

CREATE TABLE IF NOT EXISTS migration_job_approvals (
    job_id BIGINT NOT NULL REFERENCES migration_jobs(id) ON DELETE CASCADE,
    generation INTEGER NOT NULL,
    group_id BIGINT NOT NULL,
    leader_id BIGINT NOT NULL REFERENCES users(id),
    approved_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (job_id, generation, group_id, leader_id)
);

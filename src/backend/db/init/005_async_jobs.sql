CREATE TABLE IF NOT EXISTS migration_jobs (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    database_id BIGINT NOT NULL REFERENCES managed_databases(id) ON DELETE CASCADE,
    version BIGINT NOT NULL,
    user_id BIGINT NOT NULL REFERENCES users(id),
    direction TEXT NOT NULL CHECK (direction IN ('up','rollback','forward')),
    mode TEXT NOT NULL CHECK (mode IN ('normal','batch')),
    batch_table TEXT,
    batch_size INTEGER CHECK (batch_size BETWEEN 1 AND 10000),
    sql_text TEXT NOT NULL,
    source_job_id BIGINT REFERENCES migration_jobs(id),
    traceparent TEXT,
    tracestate TEXT,
    status TEXT NOT NULL DEFAULT 'pending_review' CHECK (status IN ('pending_review','pending','running','success','failed','partial','uncertain','partially_reverted','cancelled')),
    attempt INTEGER NOT NULL DEFAULT 0,
    batches_done BIGINT NOT NULL DEFAULT 0,
    rows_done BIGINT NOT NULL DEFAULT 0,
    error_message TEXT,
    available_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    claimed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ
);
ALTER TABLE migration_jobs ADD COLUMN IF NOT EXISTS traceparent TEXT;
ALTER TABLE migration_jobs ADD COLUMN IF NOT EXISTS tracestate TEXT;
ALTER TABLE migration_jobs DROP CONSTRAINT IF EXISTS migration_jobs_status_check;
ALTER TABLE migration_jobs ADD CONSTRAINT migration_jobs_status_check CHECK (status IN ('pending_review','pending','running','success','failed','partial','uncertain','partially_reverted','cancelled'));
CREATE UNIQUE INDEX IF NOT EXISTS migration_jobs_blocking_database_v2 ON migration_jobs(database_id)
    WHERE status IN ('pending_review','pending','running','partial','uncertain','partially_reverted');
CREATE INDEX IF NOT EXISTS migration_jobs_ready ON migration_jobs(available_at,id) WHERE status = 'pending';
CREATE TABLE IF NOT EXISTS migration_job_approvals (
 job_id BIGINT NOT NULL REFERENCES migration_jobs(id) ON DELETE CASCADE,
 group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
 leader_user_id BIGINT NOT NULL REFERENCES users(id),
 approved_at TIMESTAMPTZ,
 PRIMARY KEY(job_id,group_id)
);

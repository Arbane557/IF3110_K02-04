-- Keep observability timestamps separate from delivery backoff and approval time.
ALTER TABLE migration_jobs
    ADD COLUMN IF NOT EXISTS queued_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN IF NOT EXISTS lock_wait_started_at TIMESTAMPTZ;

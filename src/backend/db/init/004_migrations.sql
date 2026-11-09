CREATE TABLE migrations (
    database_id BIGINT NOT NULL REFERENCES managed_databases(id) ON DELETE CASCADE,
    version BIGINT NOT NULL,
    sql_text TEXT NOT NULL,
    created_by BIGINT NOT NULL REFERENCES users(id),
    executed_by BIGINT REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    executed_at TIMESTAMPTZ,
    status TEXT NOT NULL CHECK (status IN ('draft', 'running', 'success', 'failed')),
    error_message TEXT,
    PRIMARY KEY (database_id, version)
);

CREATE INDEX migrations_database_status_idx
    ON migrations (database_id, status);

CREATE TABLE migrations (
    database_id BIGINT NOT NULL REFERENCES managed_databases(id) ON DELETE CASCADE,
    version BIGINT NOT NULL,
    sql_text TEXT NOT NULL,
    sql_down TEXT,
    reversible BOOLEAN NOT NULL DEFAULT FALSE,
    irreversible_reason TEXT,
    active BOOLEAN NOT NULL DEFAULT FALSE,
    created_by BIGINT NOT NULL REFERENCES users(id),
    executed_by BIGINT REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    executed_at TIMESTAMPTZ,
    status TEXT NOT NULL CHECK (status IN ('draft', 'running', 'success', 'failed', 'rolled_back')),
    error_message TEXT,
    PRIMARY KEY (database_id, version)
);

CREATE INDEX migrations_database_status_idx
    ON migrations (database_id, status);

CREATE TABLE migration_history (
    execution_id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    database_id BIGINT NOT NULL REFERENCES managed_databases(id) ON DELETE CASCADE,
    version BIGINT NOT NULL,
    user_id BIGINT NOT NULL REFERENCES users(id),
    direction TEXT NOT NULL CHECK (direction IN ('up', 'forward', 'rollback')),
    status TEXT NOT NULL CHECK (status IN ('running', 'success', 'failed')),
    sql_text TEXT NOT NULL,
    execution_started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    executed_at TIMESTAMPTZ,
    error_message TEXT,
    cancelled_execution_id BIGINT REFERENCES migration_history(execution_id)
);

CREATE INDEX migration_history_database_idx
    ON migration_history (database_id, execution_id DESC);

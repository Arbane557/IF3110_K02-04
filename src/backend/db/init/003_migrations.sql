CREATE TABLE platform_migrations (
    version BIGINT PRIMARY KEY,
    sql_text TEXT NOT NULL,
    created_by BIGINT NOT NULL,
    executed_by BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    executed_at TIMESTAMPTZ,
    status TEXT NOT NULL CHECK (status IN ('draft', 'running', 'success', 'failed')),
    error_message TEXT
);

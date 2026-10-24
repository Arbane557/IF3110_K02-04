CREATE TABLE managed_databases (
    id BIGSERIAL PRIMARY KEY,
    name VARCHAR(100) NOT NULL UNIQUE,
    host TEXT NOT NULL,
    port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    database_name TEXT NOT NULL,
    username TEXT NOT NULL,
    ssl_mode TEXT NOT NULL CHECK (ssl_mode IN (
        'disable', 'allow', 'prefer', 'require', 'verify-ca', 'verify-full'
    )),
    password_ciphertext BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

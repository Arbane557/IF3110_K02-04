CREATE TABLE managed_databases (
    id BIGSERIAL PRIMARY KEY,
    name VARCHAR(100) NOT NULL UNIQUE,
    host TEXT NOT NULL,
    port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    database_name TEXT NOT NULL,
    username TEXT NOT NULL,
    ssl_mode TEXT NOT NULL DEFAULT 'require' CHECK (ssl_mode IN (
        'disable', 'allow', 'prefer', 'require', 'verify-ca', 'verify-full'
    )),
    access_roles TEXT[] NOT NULL DEFAULT ARRAY['ADMIN']::TEXT[]
        CHECK (
            cardinality(access_roles) > 0
            AND access_roles <@ ARRAY['ADMIN', 'USER']::TEXT[]
        ),
    password_ciphertext BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

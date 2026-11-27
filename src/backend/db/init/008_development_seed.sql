CREATE EXTENSION IF NOT EXISTS pgcrypto;

INSERT INTO users (username, password_hash, is_admin)
VALUES (
    'admin',
    crypt('admin1234', gen_salt('bf', 12)),
    TRUE
)
ON CONFLICT (username) DO UPDATE
SET password_hash = EXCLUDED.password_hash,
    is_admin = TRUE;

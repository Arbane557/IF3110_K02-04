-- Placeholder admin bingung wak
docker compose exec -T database psql -U app -d app <<'SQL'
CREATE EXTENSION IF NOT EXISTS pgcrypto;

INSERT INTO users (username, password_hash, is_admin)
VALUES ('admin', crypt('hytam', gen_salt('bf', 12)), TRUE)
ON CONFLICT (username) DO UPDATE
SET password_hash = EXCLUDED.password_hash,
    is_admin = TRUE;
SQL
-- Placeholder admin bingung wak
docker compose exec -T database psql -U app -d app <<'SQL'
CREATE EXTENSION IF NOT EXISTS pgcrypto;

INSERT INTO users (username, password_hash, is_admin)
VALUES ('admin', crypt('hytam', gen_salt('bf', 12)), TRUE)
ON CONFLICT (username) DO UPDATE
SET password_hash = EXCLUDED.password_hash,
    is_admin = TRUE;
SQL

## Database access roles

Each registry entry has an `access_roles` field with `ADMIN`, `USER`, or both, defined in `src/backend/db/init/002_database_registry.sql`.

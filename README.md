-- Placeholder admin bingung wak
docker compose exec -T database psql -U app -d app <<'SQL'
CREATE EXTENSION IF NOT EXISTS pgcrypto;

INSERT INTO users (username, password_hash, is_admin)
VALUES ('admin', crypt('hytam', gen_salt('bf', 12)), TRUE)
ON CONFLICT (username) DO UPDATE
SET password_hash = EXCLUDED.password_hash,
    is_admin = TRUE;
SQL

## Database access

Platform admins manage users and groups from `/auth` and `/groups`. Open a group to add or remove users and set each database to **View Only**, **View + Edit**, or **No access**. Group management endpoints are under `/api/auth/groups` and require a platform admin session.

Database access is assigned through groups in `src/backend/db/init/003_access_control.sql`. When a user belongs to multiple groups, the highest permission applies: **View + Edit** wins over **View Only**, and no matching grant means no access.
Target database passwords are encrypted in the platform registry using one application key.

Create a private `.env` file once, and keep that same key for future restarts:

```bash
printf 'CREDENTIAL_ENCRYPTION_KEY=%s\n' "$(openssl rand -hex 32)" > .env
docker compose up --build
```

For the new registry schema, recreate the local Compose database with `docker compose down -v` before starting it again. This deletes local Compose volume data.

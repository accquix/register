#!/bin/sh
# Moves a deployment created before the roles were separated (one superuser "register" used by
# every process) to the separate roles, keeping the data, the objections and the API keys.
#
#   1. docker compose down                 (keeps the pgdata volume)
#   2. ./make-secrets.sh --upgrade         (adds the new secret files; keeps the old ones)
#   3. ./upgrade-roles.sh                  (this script)
#   4. docker compose up -d
#
# It starts only PostgreSQL, creates the roles, hands the database and everything in it to
# register_migrate, and stops. `migrate` then grants the table rights on the next start.
# Safe to run again.
set -eu
cd "$(dirname "$0")"
for f in postgres_password migrate_password sync_password operator_password api_password; do
  [ -f "secrets/$f" ] || { echo "secrets/$f is missing: run ./make-secrets.sh --upgrade first" >&2; exit 1; }
done
docker compose up -d --wait postgres
# The old superuser was "register"; a new data directory has "postgres".
if docker compose exec -T postgres psql -U register -d register -Atc 'select 1' >/dev/null 2>&1; then admin=register; else admin=postgres; fi
run() { docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -U "$admin" -d register "$@"; }
docker compose exec -T postgres sh -c 'exec psql -v ON_ERROR_STOP=1 -U "$0" -d register -v db=register \
  -v migrate_pw="$(cat /run/secrets/migrate_password)" -v sync_pw="$(cat /run/secrets/sync_password)" \
  -v operator_pw="$(cat /run/secrets/operator_password)" -v api_pw="$(cat /run/secrets/api_password)" \
  -f /docker-entrypoint-initdb.d/roles.sql.in' "$admin" >/dev/null
# Everything the register created in the public schema goes to the new owner. (REASSIGN OWNED
# cannot be used: the old owner is the bootstrap superuser.)
run -q <<'SQL'
SELECT format('ALTER %s %I.%I OWNER TO register_migrate',
              CASE c.relkind WHEN 'S' THEN 'SEQUENCE' WHEN 'v' THEN 'VIEW' ELSE 'TABLE' END, n.nspname, c.relname)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public' AND c.relkind IN ('r', 'S', 'v', 'p')
  AND pg_get_userbyid(c.relowner) <> 'register_migrate'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype IN ('e', 'i', 'a'))
\gexec
SQL
echo "roles ready (administrator: $admin). Start the stack with: docker compose up -d"
if [ "$admin" = register ]; then
  echo "Note: the superuser of this data directory is still \"register\" with the password in secrets/postgres_password."
  echo "No register process uses it any more; keep it for administration only."
fi

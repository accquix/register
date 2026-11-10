#!/bin/sh
# Runs once, when PostgreSQL initialises an empty data directory. It creates the three roles of
# docs/self-hosting.md, so that no register process connects as the superuser:
#   register_migrate  owns the database and the schema; runs `migrate`
#   register_sync     reads and writes the tables; runs `sync` and `suppress`
#   register_api      SELECT only, read-only transactions; runs `serve`
# The superuser (POSTGRES_USER) is used only here and by the operator.
set -eu
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
  -v db="$POSTGRES_DB" \
  -v migrate_pw="$(cat /run/secrets/migrate_password)" \
  -v sync_pw="$(cat /run/secrets/sync_password)" \
  -v api_pw="$(cat /run/secrets/api_password)" <<'SQL'
CREATE ROLE register_migrate LOGIN PASSWORD :'migrate_pw';
CREATE ROLE register_sync    LOGIN PASSWORD :'sync_pw';
CREATE ROLE register_api     LOGIN PASSWORD :'api_pw';

-- The owner may create objects in the public schema and the trusted pg_trgm extension.
ALTER DATABASE :"db" OWNER TO register_migrate;
REVOKE ALL ON DATABASE :"db" FROM PUBLIC;
GRANT CONNECT ON DATABASE :"db" TO register_sync, register_api;
GRANT TEMPORARY ON DATABASE :"db" TO register_sync;          -- the sync uses temporary tables

-- Rights on everything `migrate` creates, now and in later versions.
ALTER DEFAULT PRIVILEGES FOR ROLE register_migrate IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE, MAINTAIN ON TABLES TO register_sync;
ALTER DEFAULT PRIVILEGES FOR ROLE register_migrate IN SCHEMA public
  GRANT USAGE ON SEQUENCES TO register_sync;
ALTER DEFAULT PRIVILEGES FOR ROLE register_migrate IN SCHEMA public
  GRANT SELECT ON TABLES TO register_api;

-- The API never writes; a runaway query is cut by the server too, not only by the client.
ALTER ROLE register_api SET default_transaction_read_only = on;
ALTER ROLE register_api SET statement_timeout = '10s';
SQL

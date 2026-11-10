#!/bin/sh
# Runs once, when PostgreSQL initialises an empty data directory. It creates the roles of
# docs/self-hosting.md, so that no register process connects as the superuser:
#   register_migrate   owns the database and the schema; runs `migrate`, which also grants the
#                      other roles exactly the table rights they need (the privilege matrix)
#   register_sync      runs `sync`: writes the mirror and the search layer, reads the objections
#   register_operator  runs `suppress`: changes the objections
#   register_api       runs `serve`: reads the published data, nothing else
# The superuser (POSTGRES_USER) is used only here and by the operator of the host.
# For a data directory created before this layout, see upgrade-roles.sh.
set -eu
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
  -v db="$POSTGRES_DB" \
  -v migrate_pw="$(cat /run/secrets/migrate_password)" \
  -v sync_pw="$(cat /run/secrets/sync_password)" \
  -v operator_pw="$(cat /run/secrets/operator_password)" \
  -v api_pw="$(cat /run/secrets/api_password)" \
  -f /docker-entrypoint-initdb.d/roles.sql.in

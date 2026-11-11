#!/bin/sh
# Creates the secret files for compose.yaml with random values and prints the API key once.
# Existing files are never overwritten.
#   ./make-secrets.sh            a new deployment
#   ./make-secrets.sh --upgrade  a deployment from before the roles were separated: keeps
#                                postgres_password and api_keys, sets the old superuser URL aside
#                                and adds the new files; then run ./upgrade-roles.sh
set -eu
cd "$(dirname "$0")"
# The directory is private (0700); the files inside must be readable (0644) because compose
# bind-mounts them as they are and the containers do not run as your user (the register runs as
# uid 65532). Other users of the host cannot reach them through the private directory.
mkdir -p secrets
chmod 700 secrets
umask 022

# Before the roles were separated, one superuser URL served every process.
if [ -f secrets/database_url ] && ! grep -q '//register_api:' secrets/database_url; then
  if [ "${1:-}" != "--upgrade" ]; then
    echo "secrets/database_url is from the old single-role layout (the API connected as the superuser)." >&2
    echo "To keep your data and objections: ./make-secrets.sh --upgrade, then ./upgrade-roles.sh" >&2
    echo "(docs/self-hosting.md, \"Upgrading from the single-role layout\")." >&2
    exit 1
  fi
  mv secrets/database_url secrets/database_url.single-role.old
  echo "kept the old superuser URL as secrets/database_url.single-role.old (delete it once the upgrade works)"
fi

random() { head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'; }

# postgres_password is the superuser's: used by initdb and by you, never by the register.
for name in postgres migrate sync operator api; do
  [ -f "secrets/${name}_password" ] || random 32 > "secrets/${name}_password"
done
url() { printf 'postgres://register_%s:%s@postgres:5432/register?sslmode=disable' "$1" "$(cat "secrets/$1_password")"; }
[ -f secrets/migrate_database_url ] || url migrate > secrets/migrate_database_url
[ -f secrets/sync_database_url ] || url sync > secrets/sync_database_url
[ -f secrets/operator_database_url ] || url operator > secrets/operator_database_url
[ -f secrets/database_url ] || url api > secrets/database_url

if [ ! -f secrets/api_keys ]; then
  key=$(random 24)
  if command -v sha256sum >/dev/null 2>&1; then hash=$(printf %s "$key" | sha256sum | cut -d' ' -f1)
  else hash=$(printf %s "$key" | shasum -a 256 | cut -d' ' -f1); fi
  printf 'default:%s' "$hash" > secrets/api_keys
  echo "API key (shown once, store it now): $key"
fi
chmod 644 secrets/*_password secrets/*database_url secrets/api_keys
echo "secrets ready in $(pwd)/secrets"

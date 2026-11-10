#!/bin/sh
# Creates the secret files for compose.yaml with random values and prints the API key once.
# Existing files are never overwritten.
set -eu
cd "$(dirname "$0")"
mkdir -p secrets
umask 077
if [ ! -f secrets/postgres_password ]; then
  head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > secrets/postgres_password
fi
if [ ! -f secrets/database_url ]; then
  printf 'postgres://register:%s@postgres:5432/register?sslmode=disable' "$(cat secrets/postgres_password)" > secrets/database_url
fi
if [ ! -f secrets/api_keys ]; then
  key=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
  if command -v sha256sum >/dev/null 2>&1; then hash=$(printf %s "$key" | sha256sum | cut -d' ' -f1)
  else hash=$(printf %s "$key" | shasum -a 256 | cut -d' ' -f1); fi
  printf 'default:%s' "$hash" > secrets/api_keys
  echo "API key (shown once, store it now): $key"
fi
echo "secrets ready in $(pwd)/secrets"

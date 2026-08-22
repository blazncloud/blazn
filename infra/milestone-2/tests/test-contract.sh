#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$TEST_DIR/.." && pwd)
compose=$ROOT_DIR/compose.yaml
ngrok=$ROOT_DIR/ngrok.example.yml
unit=$ROOT_DIR/systemd/blazn-control-plane.service

# The first four strings intentionally assert unexpanded Compose interpolation.
# shellcheck disable=SC2016
for expected in \
  '127.0.0.1}:${POSTGRES_PORT:-55432}:5432' \
  '127.0.0.1}:${S3_PORT:-59000}:9000' \
  '127.0.0.1}:${S3_CONSOLE_PORT:-59001}:9001' \
  '127.0.0.1}:${API_PORT:-58080}:8080' \
  'context: ../../services/control-api' \
  'command: ["node", "dist/migrate.js"]' \
  'MIGRATION_DATABASE_URL_FILE: /run/secrets/migration_database_url' \
  'command: ["node", "dist/bootstrap.js"]' \
  'BOOTSTRAP_DATABASE_URL_FILE: /run/secrets/bootstrap_database_url' \
  'BLAZN_INITIAL_PASSWORD_FILE: /run/secrets/initial_password' \
  'BIND_ADDRESS: 0.0.0.0' \
  'DATABASE_URL_FILE: /run/secrets/runtime_database_url' \
  'PUBLIC_URL: ${PUBLIC_URL:-http://127.0.0.1:58080}' \
  'S3_ENDPOINT: http://object:9000' \
  'S3_ACCESS_KEY_FILE: /run/secrets/s3_access_key' \
  'S3_SECRET_KEY_FILE: /run/secrets/s3_secret_key'; do
  grep -F "$expected" "$compose" >/dev/null || {
    printf 'compose contract is missing: %s\n' "$expected" >&2
    exit 1
  }
done

runtime_api=$(awk '
  /^  api:$/ { in_api=1; next }
  in_api && /^[^ ]/ { exit }
  in_api { print }
' "$compose")
for forbidden in MIGRATION_DATABASE_URL_FILE BOOTSTRAP_DATABASE_URL_FILE BLAZN_INITIAL_PASSWORD_FILE migration_database_url initial_password; do
  if printf '%s\n' "$runtime_api" | grep -F "$forbidden" >/dev/null; then
    printf 'runtime API receives a privileged bootstrap/migration secret: %s\n' "$forbidden" >&2
    exit 1
  fi
done

grep -F 'CREATE ROLE blazn_migration' "$ROOT_DIR/postgres-init/01-roles.sh" >/dev/null
grep -F 'CREATE ROLE blazn_runtime' "$ROOT_DIR/postgres-init/01-roles.sh" >/dev/null
grep -F 'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO blazn_runtime' "$ROOT_DIR/postgres-init/01-roles.sh" >/dev/null
if grep -E 'SUPERUSER|CREATEDB|CREATEROLE|REPLICATION' "$ROOT_DIR/postgres-init/01-roles.sh" | grep -v 'NO' >/dev/null; then
  printf 'restricted database roles receive an administrative capability\n' >&2
  exit 1
fi

grep -F 'addr: http://127.0.0.1:58080' "$ngrok" >/dev/null
grep -F 'domain: blazn.benpelo.com' "$ngrok" >/dev/null
# This is intentionally the literal shell-style interpolation token.
# shellcheck disable=SC2016
if grep -F '${NGROK_AUTHTOKEN}' "$ngrok" >/dev/null; then
  printf 'ngrok config incorrectly relies on shell interpolation\n' >&2
  exit 1
fi
if grep -E '^[[:space:]]+addr:' "$ngrok" | grep -Ev 'http://127\.0\.0\.1:58080$' >/dev/null; then
  printf 'ngrok config exposes a data-plane endpoint\n' >&2
  exit 1
fi
grep -F 'Environment=DOCKER_CONFIG=/etc/blazn/docker-cli' "$unit" >/dev/null

for script in "$ROOT_DIR"/scripts/*.sh "$ROOT_DIR"/postgres-init/*.sh "$ROOT_DIR"/tests/*.sh; do
  sh -n "$script"
done

printf 'infrastructure contract tests passed\n'

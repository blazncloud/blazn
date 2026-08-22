#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$TEST_DIR/.." && pwd)
compose=$ROOT_DIR/compose.yaml
ngrok=$ROOT_DIR/ngrok.example.yml
unit=$ROOT_DIR/systemd/blazn-control-plane.service
ngrok_unit=$ROOT_DIR/systemd/blazn-ngrok.service

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
  'S3_ACCESS_KEY_FILE: /run/secrets/s3_runtime_access_key' \
  'S3_SECRET_KEY_FILE: /run/secrets/s3_runtime_secret_key'; do
  grep -F "$expected" "$compose" >/dev/null || {
    printf 'compose contract is missing: %s\n' "$expected" >&2
    exit 1
  }
done

# These strings intentionally assert unexpanded Compose interpolation.
# shellcheck disable=SC2016
for expected in \
  'object-init:' \
  'MC_CONFIG_DIR: /tmp/mc' \
  'mc mb --ignore-existing "blazn/${S3_BUCKET:-blazn-poc}"' \
  'mc stat "blazn/${S3_BUCKET:-blazn-poc}"' \
  'mc admin policy create blazn blazn-runtime' \
  'mc admin user add blazn "$$runtime_access" "$$runtime_secret"' \
  'object-init:' \
  'condition: service_completed_successfully' \
  'blazn.dev/restart-idempotent: "true"'; do
  grep -F "$expected" "$compose" >/dev/null || {
    printf 'one-shot initialization contract is missing: %s\n' "$expected" >&2
    exit 1
  }
done

runtime_api=$(awk '
  /^  api:$/ { in_api=1; next }
  in_api && /^[^ ]/ { exit }
  in_api { print }
' "$compose")
printf '%s\n' "$runtime_api" | grep -F 'object-init:' >/dev/null
printf '%s\n' "$runtime_api" | grep -F 'condition: service_completed_successfully' >/dev/null
for forbidden in MIGRATION_DATABASE_URL_FILE BOOTSTRAP_DATABASE_URL_FILE BLAZN_INITIAL_PASSWORD_FILE migration_database_url bootstrap_database_url initial_password s3_root_access_key s3_root_secret_key; do
  if printf '%s\n' "$runtime_api" | grep -F "$forbidden" >/dev/null; then
    printf 'runtime API receives a privileged bootstrap/migration secret: %s\n' "$forbidden" >&2
    exit 1
  fi
done

grep -F 'CREATE ROLE blazn_migration' "$ROOT_DIR/postgres-init/01-roles.sh" >/dev/null
grep -F 'CREATE ROLE blazn_runtime' "$ROOT_DIR/postgres-init/01-roles.sh" >/dev/null
grep -F 'CREATE ROLE blazn_bootstrap' "$ROOT_DIR/postgres-init/01-roles.sh" >/dev/null
if grep -F 'ALTER DEFAULT PRIVILEGES' "$ROOT_DIR/postgres-init/01-roles.sh" >/dev/null; then
  printf 'database initialization grants broad future-table privileges\n' >&2
  exit 1
fi
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
grep -F 'Environment=COMPOSE_BAKE=false' "$unit" >/dev/null
grep -F 'ExecStart=/opt/blazn/infra/milestone-2/scripts/run-control-plane.sh' "$unit" >/dev/null
grep -F 'Restart=on-failure' "$unit" >/dev/null
if grep -F 'restart: unless-stopped' "$compose" >/dev/null; then
  printf 'Docker still owns a control-plane restart policy\n' >&2
  exit 1
fi
grep -F -- '--url https://blazn.benpelo.com' "$ngrok_unit" >/dev/null
grep -F 'with-public-origin-lock.sh permanent' "$ngrok_unit" >/dev/null
grep -F -- '--inspect=false' "$ngrok_unit" >/dev/null
grep -F '127.0.0.1:58080' "$ngrok_unit" >/dev/null
grep -F 'export DOCKER_CONFIG=' "$ROOT_DIR/scripts/backup.sh" >/dev/null
grep -F 'export DOCKER_CONFIG=' "$ROOT_DIR/scripts/verify-object-store.sh" >/dev/null
grep -F 'assert_approved_backup_mount' "$ROOT_DIR/scripts/backup.sh" >/dev/null
# These assertions intentionally match literal shell variables in preflight.
# shellcheck disable=SC2016
grep -F 'assert_directory_owned_mode "$SECRETS_ROOT" 0 700' "$ROOT_DIR/scripts/preflight.sh" >/dev/null
# shellcheck disable=SC2016
grep -F 'assert_regular_file_owned_mode "$SECRETS_ROOT/$secret" 0 444' "$ROOT_DIR/scripts/preflight.sh" >/dev/null
grep -F 'objects.before.jsonl' "$ROOT_DIR/scripts/backup.sh" >/dev/null
grep -F 'objects.after.jsonl' "$ROOT_DIR/scripts/backup.sh" >/dev/null
grep -F 'configUpdatedAt' "$ROOT_DIR/ownership-receipt.schema.json" >/dev/null
grep -F 'with-public-origin-lock.sh qualification' "$ROOT_DIR/systemd/blazn-ngrok-qualification.service" >/dev/null

boundary_tmp=${TMPDIR:-/tmp}/blazn-restore-boundary-$$
restore_parent_created=0
if [ ! -d /var/tmp/blazn-restore ]; then
  mkdir /var/tmp/blazn-restore
  restore_parent_created=1
fi
mkdir "$boundary_tmp"
cleanup_boundary() {
  rm -f "$boundary_tmp/out" "$boundary_tmp/err"
  rmdir "$boundary_tmp" 2>/dev/null || true
  [ "$restore_parent_created" -eq 0 ] || rmdir /var/tmp/blazn-restore 2>/dev/null || true
}
trap cleanup_boundary EXIT HUP INT TERM
if "$ROOT_DIR/scripts/restore-test.sh" "$boundary_tmp" "/var/tmp/blazn-restore/../blazn-restore-escape-$$" >"$boundary_tmp/out" 2>"$boundary_tmp/err"; then
  printf 'restore traversal boundary unexpectedly passed\n' >&2
  exit 1
fi
grep -F 'direct child' "$boundary_tmp/err" >/dev/null
cleanup_boundary
trap - EXIT HUP INT TERM

nofollow_tmp=${TMPDIR:-/tmp}/blazn-nofollow-contract-$$
mkdir "$nofollow_tmp" "$nofollow_tmp/real-directory"
touch "$nofollow_tmp/real-secret"
chmod 0444 "$nofollow_tmp/real-secret"
ln -s "$nofollow_tmp/real-secret" "$nofollow_tmp/linked-secret"
ln -s "$nofollow_tmp/real-directory" "$nofollow_tmp/linked-directory"
current_uid=$(id -u)
if (
  # shellcheck disable=SC1091
  . "$ROOT_DIR/scripts/common.sh"
  assert_regular_file_owned_mode "$nofollow_tmp/linked-secret" "$current_uid" 444
) 2>/dev/null; then
  printf 'symlinked secret unexpectedly passed no-follow validation\n' >&2
  exit 1
fi
if (
  # shellcheck disable=SC1091
  . "$ROOT_DIR/scripts/common.sh"
  assert_directory_owned_mode "$nofollow_tmp/linked-directory" "$current_uid" 700,2700
) 2>/dev/null; then
  printf 'symlinked data directory unexpectedly passed no-follow validation\n' >&2
  exit 1
fi
rm -f "$nofollow_tmp/linked-secret" "$nofollow_tmp/linked-directory" "$nofollow_tmp/real-secret"
rmdir "$nofollow_tmp/real-directory" "$nofollow_tmp"

for script in "$ROOT_DIR"/scripts/*.sh "$ROOT_DIR"/postgres-init/*.sh "$ROOT_DIR"/tests/*.sh; do
  sh -n "$script"
done

printf 'infrastructure contract tests passed\n'

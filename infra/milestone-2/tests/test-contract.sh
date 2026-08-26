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
  'TRUSTED_PROXY_CIDRS: 172.18.0.1/32' \
  'TRUSTED_PROXY_HOPS: "1"' \
  'TRUSTED_PROXY_SECRET_FILE: /run/secrets/proxy_auth_secret' \
  'WORKSPACE_INVITATION_HMAC_KEY_FILE: /run/secrets/workspace_invitation_hmac_v1' \
  'NODE_ENROLLMENT_HMAC_FILE: /run/secrets/node_enrollment_hmac_v1' \
  'S3_ENDPOINT: http://object:9000' \
  'S3_ACCESS_KEY_FILE: /run/secrets/s3_runtime_access_key' \
  'S3_SECRET_KEY_FILE: /run/secrets/s3_runtime_secret_key'; do
  grep -F "$expected" "$compose" >/dev/null || {
    printf 'compose contract is missing: %s\n' "$expected" >&2
    exit 1
  }
done
grep -F 'subnet: 172.18.0.0/16' "$compose" >/dev/null
grep -F 'gateway: 172.18.0.1' "$compose" >/dev/null
# This intentionally asserts the literal required Compose interpolation.
# shellcheck disable=SC2016
grep -F 'image: ${CONTROL_API_IMAGE:?set CONTROL_API_IMAGE from the verified build receipt}' "$compose" >/dev/null

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
  in_api && /^  [a-zA-Z0-9_-]+:$/ { exit }
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
printf '%s\n' "$runtime_api" | grep -F -- '- workspace_invitation_hmac_v1' >/dev/null
printf '%s\n' "$runtime_api" | grep -F -- '- node_enrollment_hmac_v1' >/dev/null
# This intentionally asserts the literal required Compose interpolation.
# shellcheck disable=SC2016
grep -F 'file: /run/blazn/identity-secrets/node-enrollment-hmac-v1' "$compose" >/dev/null
for privileged_service in api-migrate api-bootstrap database-role-compat database-role-hardening postgres object object-init object-client; do
  service_block=$(awk -v service="$privileged_service" '
    $0 == "  " service ":" { in_service=1; next }
    in_service && /^  [a-zA-Z0-9_-]+:$/ { exit }
    in_service { print }
  ' "$compose")
  if printf '%s\n' "$service_block" | grep -F 'workspace_invitation_hmac_v1' >/dev/null; then
    printf 'workspace invitation HMAC key reaches non-runtime service: %s\n' "$privileged_service" >&2
    exit 1
  fi
  if printf '%s\n' "$service_block" | grep -F 'node_enrollment_hmac_v1' >/dev/null; then
    printf 'node enrollment HMAC key reaches non-runtime service: %s\n' "$privileged_service" >&2
    exit 1
  fi
done

role_compat=$(awk '
  /^  database-role-compat:$/ { in_service=1; next }
  in_service && /^  [a-zA-Z0-9_-]+:$/ { exit }
  in_service { print }
' "$compose")
role_hardening=$(awk '
  /^  database-role-hardening:$/ { in_service=1; next }
  in_service && /^  [a-zA-Z0-9_-]+:$/ { exit }
  in_service { print }
' "$compose")
for required in 'postgres_password' 'read_only: true' 'no-new-privileges:true' 'condition: service_healthy'; do
  printf '%s\n' "$role_compat" | grep -F "$required" >/dev/null || {
    printf 'database role compatibility service is missing: %s\n' "$required" >&2
    exit 1
  }
done
for required in 'postgres_password' 'read_only: true' 'no-new-privileges:true' 'api-migrate:'; do
  printf '%s\n' "$role_hardening" | grep -F "$required" >/dev/null || {
    printf 'database role hardening service is missing: %s\n' "$required" >&2
    exit 1
  }
done
grep -F 'database-role-compat:' "$compose" >/dev/null
grep -F 'condition: service_completed_successfully' "$compose" >/dev/null
grep -F "ARRAY['blazn_sandbox_controller','blazn_development_controller']" "$ROOT_DIR/postgres-compat/ensure-controller-roles.sh" >/dev/null
grep -F 'controller role % has unsafe attributes' "$ROOT_DIR/postgres-compat/ensure-controller-roles.sh" >/dev/null
grep -F 'REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA public FROM PUBLIC' "$ROOT_DIR/postgres-compat/ensure-controller-roles.sh" >/dev/null
grep -F "ARRAY['public.digest(bytea,text)','public.digest(text,text)']" "$ROOT_DIR/postgres-compat/ensure-controller-roles.sh" >/dev/null

grep -F 'CREATE ROLE blazn_migration' "$ROOT_DIR/postgres-init/01-roles.sh" >/dev/null
grep -F 'CREATE ROLE blazn_runtime' "$ROOT_DIR/postgres-init/01-roles.sh" >/dev/null
grep -F 'CREATE ROLE blazn_bootstrap' "$ROOT_DIR/postgres-init/01-roles.sh" >/dev/null
grep -F 'NOBYPASSRLS' "$ROOT_DIR/postgres-init/01-roles.sh" >/dev/null
grep -F 'NOBYPASSRLS' "$ROOT_DIR/scripts/upgrade-live-v1-to-v2.sh" >/dev/null
grep -F 'pg_auth_members' "$ROOT_DIR/scripts/upgrade-live-v1-to-v2.sh" >/dev/null
grep -F 'openssl rand -hex 32' "$ROOT_DIR/scripts/upgrade-live-v2-to-workspace.sh" >/dev/null
grep -F 'workspace-invitation-hmac-v1' "$ROOT_DIR/scripts/verify-rollback-inventory.sh" >/dev/null
test -f "$ROOT_DIR/scripts/verify-rollback-metadata.jq"
grep -F 'POC_IDENTITY_ACTION: provision' "$compose" >/dev/null
grep -F 'POC_IDENTITY_ACTION: cleanup' "$compose" >/dev/null
grep -F 'passwordRecord' "$ROOT_DIR/../../services/control-api/src/poc-identity.ts" >/dev/null
grep -F 'workspace reference outside the exact cleanup inventory' "$ROOT_DIR/../../services/control-api/src/poc-identity.ts" >/dev/null
if grep -F "LIKE 'device-approve-account" "$ROOT_DIR/../../services/control-api/src/poc-identity.ts" >/dev/null; then
  printf 'POC cleanup incorrectly treats hashed rate-limit keys as plaintext\n' >&2
  exit 1
fi
grep -F 'setpriv --reuid=' "$ROOT_DIR/scripts/verify-live-workspace.sh" >/dev/null
grep -F -- '--clear-groups --reset-env' "$ROOT_DIR/scripts/verify-live-workspace.sh" >/dev/null
grep -F 'second CLI authenticated as a user other than the receipted POC identity' "$ROOT_DIR/scripts/verify-live-workspace.sh" >/dev/null
grep -F 'POC CLI users share a UID' "$ROOT_DIR/scripts/manage-poc-cli-users.sh" >/dev/null
grep -F 'POC CLI user has supplementary groups' "$ROOT_DIR/scripts/manage-poc-cli-users.sh" >/dev/null
grep -F 'must be exactly inactive' "$ROOT_DIR/scripts/promote-release.sh" >/dev/null
grep -F 'preflight.sh --existing-deploy' "$ROOT_DIR/workspace-live-integration-runbook.md" >/dev/null
grep -F 'verify-live-workspace.sh' "$ROOT_DIR/workspace-live-integration-runbook.md" >/dev/null
printf '%s\n' '{"error":{"code":"workspace_not_found","message":"not found"},"exitCode":1}' \
  | jq -e '.error.code=="workspace_not_found" or .error.code=="forbidden"' >/dev/null
printf '%s\n' '{"error":{"code":"permission_denied","message":"denied"},"exitCode":1}' \
  | jq -e '(.error.code=="workspace_not_found" or .error.code=="forbidden")|not' >/dev/null
grep -F 'with-control-plane-env.sh' "$ROOT_DIR/workspace-live-integration-runbook.md" >/dev/null
grep -F 'stage-release.sh' "$ROOT_DIR/workspace-live-integration-runbook.md" >/dev/null
grep -F 'infra/node/scripts/upgrade-control-plane.sh' "$ROOT_DIR/workspace-live-integration-runbook.md" >/dev/null
grep -F 'BLAZN_NODE_UPGRADE_DEFER_CONFIG=1' "$ROOT_DIR/workspace-live-integration-runbook.md" >/dev/null
grep -F 'The old main/build receipts remain byte-identical' "$ROOT_DIR/workspace-live-integration-runbook.md" >/dev/null
grep -F 'longer needs PostgreSQL' "$ROOT_DIR/workspace-live-integration-runbook.md" >/dev/null
grep -F 'while the current receipt-bound PostgreSQL container is still healthy' "$ROOT_DIR/workspace-live-integration-runbook.md" >/dev/null
grep -F 'Hold point B0' "$ROOT_DIR/workspace-live-integration-runbook.md" >/dev/null
# This intentionally asserts the literal Markdown code span.
# shellcheck disable=SC2016
grep -F '`inputs-backed-up`' "$ROOT_DIR/workspace-live-integration-runbook.md" >/dev/null
# This intentionally asserts literal shell variables in the promotion script.
# shellcheck disable=SC2016
grep -F 'cmp -s "$unit_source" "$installed_unit"' "$ROOT_DIR/scripts/promote-release.sh" >/dev/null
if grep -F 'ALTER DEFAULT PRIVILEGES' "$ROOT_DIR/postgres-init/01-roles.sh" | grep -F 'GRANT' >/dev/null; then
  printf 'database initialization grants broad future-table privileges\n' >&2
  exit 1
fi
if grep -E 'SUPERUSER|CREATEDB|CREATEROLE|REPLICATION' "$ROOT_DIR/postgres-init/01-roles.sh" | grep -v 'NO' >/dev/null; then
  printf 'restricted database roles receive an administrative capability\n' >&2
  exit 1
fi

grep -F 'REPLACE_WITH_DEDICATED_BLAZN_NGROK_TOKEN' "$ngrok" >/dev/null
# This is intentionally the literal shell-style interpolation token.
# shellcheck disable=SC2016
if grep -F '${NGROK_AUTHTOKEN}' "$ngrok" >/dev/null; then
  printf 'ngrok config incorrectly relies on shell interpolation\n' >&2
  exit 1
fi
grep -F 'Environment=DOCKER_CONFIG=/etc/blazn/docker-cli' "$unit" >/dev/null
grep -F 'Environment=COMPOSE_BAKE=false' "$unit" >/dev/null
grep -F 'BUILDX_NO_DEFAULT_ATTESTATIONS=1' "$ROOT_DIR/scripts/build-control-api.sh" >/dev/null
grep -F 'ExecStartPre=/opt/blazn/infra/milestone-2/scripts/with-control-plane-lock.sh systemd-start systemd auto /opt/blazn/infra/milestone-2/scripts/start-control-plane.sh' "$unit" >/dev/null
grep -F 'ExecStart=/opt/blazn/infra/milestone-2/scripts/run-control-plane.sh' "$unit" >/dev/null
grep -F 'ExecStopPost=/opt/blazn/infra/milestone-2/scripts/with-control-plane-lock.sh systemd-stop systemd auto' "$unit" >/dev/null
grep -F -- '-/run/lock/blazn-poc' "$unit" >/dev/null
if grep -F 'compose up' "$ROOT_DIR/scripts/run-control-plane.sh" >/dev/null; then
  printf 'monitor-only control-plane supervisor still mutates startup state\n' >&2
  exit 1
fi
grep -F 'up --detach --wait --remove-orphans' "$ROOT_DIR/scripts/start-control-plane.sh" >/dev/null
build_script=$ROOT_DIR/scripts/build-control-api.sh
# This intentionally asserts literal shell variables in the build script.
# shellcheck disable=SC2016
grep -F 'control_plane_compose "$ROOT_DIR" "$ENV_FILE" build api' "$build_script" >/dev/null
grep -F 'build api' "$build_script" >/dev/null
grep -F 'source_before=' "$build_script" >/dev/null
grep -F 'source_after=' "$build_script" >/dev/null
grep -F 'candidate_image=' "$build_script" >/dev/null
grep -F 'final_image=blazn-control-api:source-' "$build_script" >/dev/null
grep -F 'build-control-api.sh' "$ROOT_DIR/scripts/start-control-plane.sh" >/dev/null
grep -F 'services/control-api/src services/control-api/migrations packages/contracts' "$ROOT_DIR/scripts/common.sh" >/dev/null
grep -F 'compose.identity.yaml' "$ROOT_DIR/scripts/common.sh" >/dev/null
# This intentionally asserts a literal shell variable in the preflight script.
# shellcheck disable=SC2016
grep -F 'validate_identity_overlay "$ROOT_DIR"' "$ROOT_DIR/scripts/preflight.sh" >/dev/null
# The expressions are required literals in preflight.sh.
# shellcheck disable=SC2016
grep -F 'ENV_FILE=${BLAZN_CONTROL_PLANE_ENV_FILE:-/etc/blazn/control-plane/control-plane.env}' "$ROOT_DIR/scripts/preflight.sh" >/dev/null
# shellcheck disable=SC2016
grep -F 'assert_regular_file_owned_mode "$ENV_FILE" 0 600' "$ROOT_DIR/scripts/preflight.sh" >/dev/null
grep -F 'ZITADEL_REVIEWED_RELEASE' "$ROOT_DIR/scripts/common.sh" >/dev/null
grep -F 'ZITADEL_REVIEWED_ASSURANCE_POLICY_DIGEST' "$ROOT_DIR/scripts/common.sh" >/dev/null
grep -F 'ZITADEL_REVIEWED_ACR_POLICY' "$ROOT_DIR/scripts/common.sh" >/dev/null
grep -F 'ZITADEL_REVIEWED_MFA_AMR_SETS' "$ROOT_DIR/scripts/common.sh" >/dev/null
grep -F 'prepare-identity-runtime-secrets.sh' "$ROOT_DIR/scripts/start-control-plane.sh" >/dev/null
# This intentionally asserts a literal shell variable in the preparation script.
# shellcheck disable=SC2016
grep -F 'publish_node_enrollment_runtime_secret "$node_source_root" /run/blazn/identity-secrets' "$ROOT_DIR/scripts/prepare-identity-runtime-secrets.sh" >/dev/null
prepare_identity_line=$(grep -n 'prepare-identity-runtime-secrets.sh' "$ROOT_DIR/scripts/start-control-plane.sh" | cut -d: -f1)
recreate_identity_line=$(grep -n 'compose up --no-start --no-deps --force-recreate api' "$ROOT_DIR/scripts/start-control-plane.sh" | cut -d: -f1)
start_identity_line=$(grep -n 'compose up --detach --wait --remove-orphans' "$ROOT_DIR/scripts/start-control-plane.sh" | cut -d: -f1)
[ "$prepare_identity_line" -lt "$recreate_identity_line" ]
[ "$recreate_identity_line" -lt "$start_identity_line" ]
grep -F 'RuntimeDirectory=blazn/identity-secrets' "$unit" >/dev/null
grep -F 'RuntimeDirectoryMode=0700' "$unit" >/dev/null
grep -F '/run/blazn/identity-secrets/zitadel-client-secret' "$ROOT_DIR/compose.identity.yaml" >/dev/null
grep -F '/run/blazn/identity-secrets/oidc-cookie-key' "$ROOT_DIR/compose.identity.yaml" >/dev/null
for runtime_script in start-control-plane.sh run-control-plane.sh stop-control-plane.sh; do
  grep -F 'control_plane_compose' "$ROOT_DIR/scripts/$runtime_script" >/dev/null
done
# These assertions intentionally match literal shell and jq variables.
# shellcheck disable=SC2016
for required in \
  'BLAZN_CONTROL_API_BUILD_MODE:-local' \
  'prebuilt) validate_control_api_build "$ROOT_DIR"' \
  'actual_archive=sha256:' \
  'RepoTags == [$image]' \
  'archive_image_id=$(jq' \
  'tar -xOf "$archive" index.json' \
  'manifest_path=blobs/sha256/' \
  '.config.digest == $configDigest' \
  'buildMode:"prebuilt"'; do
  grep -F -- "$required" "$ROOT_DIR/scripts/start-control-plane.sh" "$ROOT_DIR/scripts/import-control-api-image.sh" >/dev/null
done
grep -F 'controlApi' "$ROOT_DIR/ownership-receipt.schema.json" >/dev/null
grep -F 'verify_control_api_containers' "$ROOT_DIR/scripts/start-control-plane.sh" >/dev/null
grep -F 'verify_control_api_containers' "$ROOT_DIR/scripts/run-control-plane.sh" >/dev/null
grep -F 'stop-control-plane.sh' "$unit" >/dev/null
grep -F 'Restart=on-failure' "$unit" >/dev/null
if grep -F 'restart: unless-stopped' "$compose" >/dev/null; then
  printf 'Docker still owns a control-plane restart policy\n' >&2
  exit 1
fi
grep -F -- '--url https://blazn.benpelo.com' "$ngrok_unit" >/dev/null
grep -F 'with-public-origin-lock.sh permanent' "$ngrok_unit" >/dev/null
grep -F '/usr/bin/setpriv --reuid=blazn-ngrok --regid=blazn-ngrok --clear-groups' "$ngrok_unit" >/dev/null
grep -F 'unreviewed supplementary group memberships' "$ROOT_DIR/scripts/install-ngrok-user.sh" >/dev/null
grep -F 'root:blazn-ngrok:710' "$ROOT_DIR/scripts/install-ngrok-user.sh" >/dev/null
grep -F -- '--traffic-policy-file /etc/blazn/ngrok/traffic-policy.yml' "$ngrok_unit" >/dev/null
grep -F 'prepare-ngrok-policy.sh' "$ngrok_unit" >/dev/null
grep -F 'install-ngrok-user.sh --validate-only' "$ngrok_unit" >/dev/null
if grep -F 'homeai.yml' "$ngrok_unit" >/dev/null; then
  printf 'Blazn ngrok unit still depends on the existing HomeAI user config\n' >&2
  exit 1
fi
policy_script=$ROOT_DIR/scripts/prepare-ngrok-policy.sh
remove_line=$(grep -n 'type: remove-headers' "$policy_script" | cut -d: -f1)
add_line=$(grep -n 'type: add-headers' "$policy_script" | cut -d: -f1)
[ "$remove_line" -lt "$add_line" ] || { printf 'proxy-auth header is not removed before injection\n' >&2; exit 1; }
grep -F 'x-blazn-proxy-authorization' "$policy_script" >/dev/null
if grep -F 'proxy_auth_secret' "$ngrok_unit" >/dev/null; then
  printf 'proxy authentication secret path leaked into ngrok argv\n' >&2
  exit 1
fi
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
grep -F 'control-plane-backup/v3' "$ROOT_DIR/scripts/backup.sh" >/dev/null
grep -F 'workspace-invitation-hmac-v1' "$ROOT_DIR/scripts/backup.sh" >/dev/null
grep -F 'controlApi' "$ROOT_DIR/backup-metadata.schema.json" >/dev/null
grep -F 'secretDigests' "$ROOT_DIR/backup-metadata.schema.json" >/dev/null
grep -F 'backup rollback inventory is invalid' "$ROOT_DIR/scripts/restore-test.sh" >/dev/null
grep -F 'backup inventory does not match' "$ROOT_DIR/scripts/verify-rollback-inventory.sh" >/dev/null
grep -F 'configUpdatedAt' "$ROOT_DIR/ownership-receipt.schema.json" >/dev/null
grep -F 'secretDigests' "$ROOT_DIR/ownership-receipt.schema.json" >/dev/null
grep -F 'control-plane-v2-upgrade/v1' "$ROOT_DIR/upgrade-receipt.schema.json" >/dev/null
grep -F 'reconcile the main ownership receipt separately' "$ROOT_DIR/scripts/upgrade-live-v1-to-v2.sh" >/dev/null
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
if "$ROOT_DIR/scripts/restore-test.sh" "$boundary_tmp" "/var/tmp/blazn-restore/../blazn-restore-escape-$$" "$boundary_tmp/receipt" "$boundary_tmp/inventory" >"$boundary_tmp/out" 2>"$boundary_tmp/err"; then
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

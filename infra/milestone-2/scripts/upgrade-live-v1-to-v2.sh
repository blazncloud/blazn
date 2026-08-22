#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "live infrastructure upgrade must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "live infrastructure upgrade must run through with-control-plane-lock.sh"
require_command docker
require_command jq
require_command openssl
require_command sha256sum
export DOCKER_CONFIG="${BLAZN_DOCKER_CONFIG_ROOT:-/etc/blazn/docker-cli}"
[ "${POSTGRES_DB:-blazn}" = blazn ] || die "live upgrade expects POSTGRES_DB=blazn"
[ "${POSTGRES_USER:-blazn_admin}" = blazn_admin ] || die "live upgrade expects POSTGRES_USER=blazn_admin"

SECRETS_ROOT=${BLAZN_SECRETS_ROOT:-/etc/blazn/control-plane/secrets}
MAIN_RECEIPT=${BLAZN_RECEIPT_PATH:-/var/lib/blazn/ownership/control-plane.json}
UPGRADE_RECEIPT=${BLAZN_UPGRADE_RECEIPT_PATH:-/var/lib/blazn/ownership/control-plane-v2-upgrade.json}
STAGE=$SECRETS_ROOT/.m2-v2-upgrade-staging

assert_directory_owned_mode "$SECRETS_ROOT" 0 700
assert_regular_file_owned_mode "$MAIN_RECEIPT" 0 600
require_absolute_path BLAZN_UPGRADE_RECEIPT_PATH "$UPGRADE_RECEIPT"
assert_not_symlink_chain "$UPGRADE_RECEIPT"
jq -e --arg host "$(hostname)" --arg secrets "$SECRETS_ROOT" \
  '.schemaVersion == "blazn.dev/control-plane-ownership/v1" and .owner == "blazn-poc" and .host == $host and .paths.secrets == $secrets' \
  "$MAIN_RECEIPT" >/dev/null || die "v1 control-plane receipt does not match this host and secrets root"

for old_secret in postgres-password migration-database-url runtime-database-url initial-password s3-access-key s3-secret-key; do
  assert_regular_file_owned_mode "$SECRETS_ROOT/$old_secret" 0 444
done

sha() {
  sha256_file "$1"
}

validate_bootstrap_url() {
  value=$(sed -n '1p' "$1")
  case "$value" in
    postgresql://blazn_bootstrap:????????????????????????????????????????????????????????????????@postgres:5432/*) ;;
    *) die "staged bootstrap database URL is invalid" ;;
  esac
  password=${value#*://*:}
  password=${password%%@*}
  case "$password" in *[!a-f0-9]*) die "staged bootstrap password is not lowercase hexadecimal" ;; esac
}

validate_runtime_access() {
  value=$(sed -n '1p' "$1")
  case "$value" in
    blaznruntime????????????????) ;;
    *) die "staged S3 runtime access key is invalid" ;;
  esac
  suffix=${value#blaznruntime}
  case "$suffix" in *[!a-f0-9]*) die "staged S3 runtime access key is not lowercase hexadecimal" ;; esac
}

validate_hex_secret() {
  value=$(sed -n '1p' "$1")
  case "$value" in
    ????????????????????????????????????????????????????????????????) ;;
    *) die "staged secret has an unexpected length" ;;
  esac
  case "$value" in
    *[!a-f0-9]*) die "staged secret is not lowercase hexadecimal" ;;
  esac
}

write_stage_value() {
  target=$1
  value=$2
  if [ -e "$target" ]; then
    assert_regular_file_owned_mode "$target" 0 444
    return
  fi
  tmp=$target.tmp.$$
  umask 077
  printf '%s\n' "$value" >"$tmp"
  chmod 0444 "$tmp"
  ln -- "$tmp" "$target" || {
    rm -f -- "$tmp"
    die "staged secret target appeared during upgrade"
  }
  rm -f -- "$tmp"
}

install_matching_file() {
  source=$1
  target=$2
  if [ -e "$target" ]; then
    assert_regular_file_owned_mode "$target" 0 444
    [ "$(sha "$source")" = "$(sha "$target")" ] || die "partial upgrade target does not match its staged source: $target"
    return
  fi
  ln -- "$source" "$target" || die "upgrade target appeared during atomic installation: $target"
  assert_regular_file_owned_mode "$target" 0 444
}

validate_upgrade_receipt() {
  assert_regular_file_owned_mode "$UPGRADE_RECEIPT" 0 600
  jq -e --arg host "$(hostname)" --arg secrets "$SECRETS_ROOT" \
    '.schemaVersion == "blazn.dev/control-plane-v2-upgrade/v1" and .owner == "blazn-poc" and .host == $host and .secretsRoot == $secrets and (.phase == "secrets-installed" or .phase == "identity-ready")' \
    "$UPGRADE_RECEIPT" >/dev/null || die "v2 upgrade receipt is invalid"
  for installed in s3-root-access-key s3-root-secret-key s3-runtime-access-key s3-runtime-secret-key bootstrap-database-url; do
    assert_regular_file_owned_mode "$SECRETS_ROOT/$installed" 0 444
    expected=$(jq -er --arg name "$installed" '.digests[$name]' "$UPGRADE_RECEIPT")
    [ "$expected" = "sha256:$(sha "$SECRETS_ROOT/$installed")" ] || die "installed secret does not match the v2 upgrade receipt: $installed"
  done
}

if [ -e "$UPGRADE_RECEIPT" ]; then
  validate_upgrade_receipt
else
  if [ ! -e "$STAGE" ]; then
    for unexpected in s3-root-access-key s3-root-secret-key s3-runtime-access-key s3-runtime-secret-key bootstrap-database-url; do
      [ ! -e "$SECRETS_ROOT/$unexpected" ] || die "new secret exists without an upgrade receipt or recovery staging directory: $unexpected"
    done
    umask 077
    mkdir -- "$STAGE"
    chmod 0700 "$STAGE"
  fi
  assert_directory_owned_mode "$STAGE" 0 700

  if [ ! -e "$STAGE/bootstrap-database-url" ]; then
    bootstrap_password=$(openssl rand -hex 32)
    write_stage_value "$STAGE/bootstrap-database-url" "postgresql://blazn_bootstrap:$bootstrap_password@postgres:5432/${POSTGRES_DB:-blazn}"
  fi
  if [ ! -e "$STAGE/s3-runtime-access-key" ]; then
    write_stage_value "$STAGE/s3-runtime-access-key" "blaznruntime$(openssl rand -hex 8)"
  fi
  if [ ! -e "$STAGE/s3-runtime-secret-key" ]; then
    write_stage_value "$STAGE/s3-runtime-secret-key" "$(openssl rand -hex 32)"
  fi
  validate_bootstrap_url "$STAGE/bootstrap-database-url"
  validate_runtime_access "$STAGE/s3-runtime-access-key"
  validate_hex_secret "$STAGE/s3-runtime-secret-key"

  install_matching_file "$SECRETS_ROOT/s3-access-key" "$SECRETS_ROOT/s3-root-access-key"
  install_matching_file "$SECRETS_ROOT/s3-secret-key" "$SECRETS_ROOT/s3-root-secret-key"
  install_matching_file "$STAGE/s3-runtime-access-key" "$SECRETS_ROOT/s3-runtime-access-key"
  install_matching_file "$STAGE/s3-runtime-secret-key" "$SECRETS_ROOT/s3-runtime-secret-key"
  install_matching_file "$STAGE/bootstrap-database-url" "$SECRETS_ROOT/bootstrap-database-url"

  mkdir -p -- "$(dirname -- "$UPGRADE_RECEIPT")"
  assert_directory_owned_mode "$(dirname -- "$UPGRADE_RECEIPT")" 0 700
  receipt_tmp=$UPGRADE_RECEIPT.tmp.$$
  created_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
  jq -cn \
    --arg host "$(hostname)" \
    --arg secrets "$SECRETS_ROOT" \
    --arg createdAt "$created_at" \
    --arg rootAccess "sha256:$(sha "$SECRETS_ROOT/s3-root-access-key")" \
    --arg rootSecret "sha256:$(sha "$SECRETS_ROOT/s3-root-secret-key")" \
    --arg runtimeAccess "sha256:$(sha "$SECRETS_ROOT/s3-runtime-access-key")" \
    --arg runtimeSecret "sha256:$(sha "$SECRETS_ROOT/s3-runtime-secret-key")" \
    --arg bootstrapUrl "sha256:$(sha "$SECRETS_ROOT/bootstrap-database-url")" \
    '{schemaVersion:"blazn.dev/control-plane-v2-upgrade/v1",owner:"blazn-poc",host:$host,secretsRoot:$secrets,phase:"secrets-installed",createdAt:$createdAt,digests:{"s3-root-access-key":$rootAccess,"s3-root-secret-key":$rootSecret,"s3-runtime-access-key":$runtimeAccess,"s3-runtime-secret-key":$runtimeSecret,"bootstrap-database-url":$bootstrapUrl}}' \
    >"$receipt_tmp"
  chmod 0600 "$receipt_tmp"
  ln -- "$receipt_tmp" "$UPGRADE_RECEIPT" || {
    rm -f -- "$receipt_tmp"
    die "upgrade receipt target appeared during installation"
  }
  rm -f -- "$receipt_tmp"
  validate_upgrade_receipt
fi

if [ -d "$STAGE" ]; then
  assert_directory_owned_mode "$STAGE" 0 700
  for staged in bootstrap-database-url s3-runtime-access-key s3-runtime-secret-key; do
    [ ! -e "$STAGE/$staged" ] || {
      assert_regular_file_owned_mode "$STAGE/$staged" 0 444
      expected=$(jq -er --arg name "$staged" '.digests[$name]' "$UPGRADE_RECEIPT")
      [ "$expected" = "sha256:$(sha "$STAGE/$staged")" ] || die "recovery staging file does not match the upgrade receipt: $staged"
      rm -f -- "$STAGE/$staged"
    }
  done
  rmdir -- "$STAGE" || die "upgrade staging directory contains an unexpected entry"
fi

compose() {
  docker compose -f "$ROOT_DIR/compose.yaml" "$@"
}
postgres_container=$(compose ps -q postgres)
[ -n "$postgres_container" ] || die "the exact Blazn PostgreSQL container is not running"
[ "$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}/{{index .Config.Labels "com.docker.compose.service"}}/{{.State.Status}}' "$postgres_container")" = "blazn-m2/postgres/running" ] || \
  die "the running PostgreSQL container is not the expected Blazn Compose service"

bootstrap_url=$(sed -n '1p' "$SECRETS_ROOT/bootstrap-database-url")
bootstrap_password=${bootstrap_url#*://*:}
bootstrap_password=${bootstrap_password%%@*}
case "$bootstrap_password" in
  ????????????????????????????????????????????????????????????????) ;;
  *) die "installed bootstrap password has an unexpected length" ;;
esac
case "$bootstrap_password" in *[!a-f0-9]*) die "installed bootstrap password is invalid" ;; esac

role_exists=$(compose exec -T postgres psql -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-blazn_admin}" -d "${POSTGRES_DB:-blazn}" -Atqc "select count(*) from pg_roles where rolname='blazn_bootstrap'")
case "$role_exists" in
  0)
    printf "CREATE ROLE blazn_bootstrap LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD '%s';\n" "$bootstrap_password" | \
      compose exec -T postgres psql -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-blazn_admin}" -d "${POSTGRES_DB:-blazn}" >/dev/null
    ;;
  1) ;;
  *) die "could not determine the existing bootstrap role state" ;;
esac
printf "ALTER ROLE blazn_bootstrap LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD '%s'; GRANT CONNECT ON DATABASE \"%s\" TO blazn_bootstrap; GRANT USAGE ON SCHEMA public TO blazn_bootstrap;\n" \
  "$bootstrap_password" "${POSTGRES_DB:-blazn}" | \
  compose exec -T postgres psql -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-blazn_admin}" -d "${POSTGRES_DB:-blazn}" >/dev/null

role_state=$(compose exec -T postgres psql -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-blazn_admin}" -d "${POSTGRES_DB:-blazn}" -Atqc \
  "select rolname,rolcanlogin,rolsuper,rolcreatedb,rolcreaterole,rolreplication from pg_roles where rolname='blazn_bootstrap'")
[ "$role_state" = "blazn_bootstrap|t|f|f|f|f" ] || die "bootstrap role attributes are not least privilege"
# The inner shell expands its positional database-name argument.
# shellcheck disable=SC2016
authenticated_user=$(printf '%s\n' "$bootstrap_password" | compose exec -T postgres /bin/sh -euc \
  'IFS= read -r PGPASSWORD; export PGPASSWORD; exec psql -h 127.0.0.1 -U blazn_bootstrap -d "$1" -Atqc "select current_user"' -- "${POSTGRES_DB:-blazn}")
[ "$authenticated_user" = blazn_bootstrap ] || die "bootstrap role credential validation failed"

identity_tmp=$UPGRADE_RECEIPT.tmp.$$
jq --arg validatedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
  '.phase="identity-ready" | .identityValidatedAt=$validatedAt' "$UPGRADE_RECEIPT" >"$identity_tmp"
chmod 0600 "$identity_tmp"
mv -- "$identity_tmp" "$UPGRADE_RECEIPT"
validate_upgrade_receipt

printf 'live v1-to-v2 infrastructure upgrade is identity-ready; reconcile the main ownership receipt separately\n'

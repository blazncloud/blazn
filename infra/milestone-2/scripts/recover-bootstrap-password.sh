#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "bootstrap password recovery must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "bootstrap password recovery must run through with-control-plane-lock.sh"
[ "$#" -eq 1 ] || die "usage: recover-bootstrap-password.sh STAGED_PASSWORD_FILE"

staged=$1
require_absolute_path STAGED_PASSWORD_FILE "$staged"
assert_not_symlink_chain "$staged"
if [ ! -f "$staged" ] || [ -L "$staged" ]; then
  die "staged password must be a non-symlink regular file"
fi
[ "$(stat -c '%u' "$staged")" = 0 ] || die "staged password must be owned by root"
staged_mode=$(stat -c '%a' "$staged")
case "$staged_mode" in
  400|440) ;;
  *) die "staged password must have mode 0400 or 0440" ;;
esac

require_command docker
require_command install
require_command cmp
require_command chmod
require_command mv
export DOCKER_CONFIG="${BLAZN_DOCKER_CONFIG_ROOT:-/etc/blazn/docker-cli}"
ENV_FILE=${BLAZN_CONTROL_PLANE_ENV_FILE:-/etc/blazn/control-plane/control-plane.env}
SECRETS_ROOT=${BLAZN_SECRETS_ROOT:-/etc/blazn/control-plane/secrets}
require_absolute_path BLAZN_CONTROL_PLANE_ENV_FILE "$ENV_FILE"
require_absolute_path BLAZN_SECRETS_ROOT "$SECRETS_ROOT"
assert_not_symlink_chain "$ENV_FILE"
assert_not_symlink_chain "$SECRETS_ROOT"
assert_regular_file_owned_mode "$ENV_FILE" 0 600
assert_directory_owned_mode "$SECRETS_ROOT" 0 700
assert_regular_file_owned_mode "$SECRETS_ROOT/bootstrap-database-url" 0 444
if [ ! -f "$SECRETS_ROOT/initial-password" ] || [ -L "$SECRETS_ROOT/initial-password" ]; then
  die "installed initial password is not a non-symlink regular file"
fi
[ "$(stat -c '%u' "$SECRETS_ROOT/initial-password")" = 0 ] || die "installed initial password must be owned by root"
[ "$(stat -c '%a' "$SECRETS_ROOT/initial-password")" = 444 ] || \
  die "installed initial password must have mode 0444"

load_control_api_image "$ROOT_DIR"
docker compose -f "$ROOT_DIR/compose.yaml" --env-file "$ENV_FILE" config --quiet >/dev/null || \
  die "control-plane Compose configuration is invalid"

candidate=$SECRETS_ROOT/.initial-password.recovery-candidate
if [ -e "$candidate" ] || [ -L "$candidate" ]; then
  if [ ! -f "$candidate" ] || [ -L "$candidate" ]; then
    die "recovery candidate must be a non-symlink regular file"
  fi
  [ "$(stat -c '%u' "$candidate")" = 0 ] || die "recovery candidate must be owned by root"
  case "$(stat -c '%a' "$candidate")" in
    400|444) ;;
    *) die "recovery candidate must have mode 0400 or 0444" ;;
  esac
  cmp -s -- "$staged" "$candidate" || die "a different recovery candidate is already pending reconciliation"
else
  install -o root -g root -m 0400 -- "$staged" "$candidate"
fi

preserve_candidate=0
recovery_exit() {
  if [ "$preserve_candidate" -eq 1 ] && [ -f "$candidate" ] && [ ! -L "$candidate" ]; then
    printf 'blazn-m2: database commit may have succeeded; preserve the recovery candidate and rerun the locked recovery command with the same staged file\n' >&2
  fi
}
recovery_signal() {
  preserve_candidate=1
  exit 1
}
trap recovery_exit EXIT
trap recovery_signal HUP INT TERM

# From this point through the atomic rename, an interrupt preserves the candidate:
# the database command may have committed even when its caller did not observe it.
preserve_candidate=1
if docker compose -f "$ROOT_DIR/compose.yaml" --env-file "$ENV_FILE" run --rm --no-deps --user 0 \
    -v "$candidate:/run/secrets/recovery-password:ro" \
    -e BLAZN_RECOVERY_PASSWORD_FILE=/run/secrets/recovery-password \
    api-bootstrap node dist/rotate-bootstrap-password.js; then
  database_status=0
else
  database_status=$?
fi
if [ "$database_status" -eq 10 ]; then
  if rm -f -- "$candidate"; then
    preserve_candidate=0
    die "bootstrap password rotation failed; the installed secret is unchanged"
  fi
  die "bootstrap password rotation failed and candidate cleanup failed; preserve and reconcile the candidate"
fi
[ "$database_status" -eq 0 ] || die "database rotation outcome is uncertain; preserve the candidate and rerun the locked recovery command"

chmod 0444 -- "$candidate" || \
  die "database rotation succeeded but candidate permission activation failed; rerun the locked recovery command with the same staged file"
mv -f -- "$candidate" "$SECRETS_ROOT/initial-password" || \
  die "database rotation succeeded but secret activation failed; rerun the locked recovery command with the same staged file"
preserve_candidate=0
trap - EXIT HUP INT TERM
printf 'bootstrap password recovery completed\n'

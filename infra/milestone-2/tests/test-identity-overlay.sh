#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$TEST_DIR/.." && pwd)
command -v sudo >/dev/null 2>&1 || { printf 'identity overlay tests skipped: sudo unavailable\n'; exit 0; }
sudo -n true >/dev/null 2>&1 || { printf 'identity overlay tests skipped: passwordless sudo unavailable\n'; exit 0; }

top=${TMPDIR:-/tmp}/blazn-identity-overlay-test-$$
mkdir -p "$top/infra"
cleanup() {
  sudo find "$top" -xdev -type f -delete
  sudo find "$top" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM
printf 'services: {}\n' >"$top/infra/compose.yaml"
printf 'services: {}\n' >"$top/infra/compose.identity.yaml"
printf 'PUBLIC_URL=https://blazn.benpelo.com\n' >"$top/control-plane.env"
printf 'ZITADEL_ISSUER_URL=https://auth.blazn.benpelo.com\n' >"$top/identity.env"
chmod 0600 "$top/identity.env"
sudo chown 0:0 "$top/identity.env"

docker() { printf '%s\n' "$*" >"$top/docker.args"; }
# shellcheck disable=SC1091
. "$ROOT_DIR/scripts/common.sh"

BLAZN_IDENTITY_ENABLED=false
export BLAZN_IDENTITY_ENABLED
control_plane_compose "$top/infra" "$top/control-plane.env" ps api
grep -Fx "compose -f $top/infra/compose.yaml --env-file $top/control-plane.env ps api" "$top/docker.args" >/dev/null

BLAZN_IDENTITY_ENABLED=true
BLAZN_IDENTITY_ENV_FILE=$top/identity.env
export BLAZN_IDENTITY_ENABLED BLAZN_IDENTITY_ENV_FILE
control_plane_compose "$top/infra" "$top/control-plane.env" ps api
grep -Fx "compose -f $top/infra/compose.yaml -f $top/infra/compose.identity.yaml --env-file $top/control-plane.env --env-file $top/identity.env ps api" "$top/docker.args" >/dev/null

BLAZN_IDENTITY_ENABLED=invalid
export BLAZN_IDENTITY_ENABLED
if (control_plane_compose "$top/infra" "$top/control-plane.env" ps api) >"$top/out" 2>"$top/err"; then
  printf 'invalid identity enablement unexpectedly passed\n' >&2
  exit 1
fi
grep -F 'BLAZN_IDENTITY_ENABLED must be true or false' "$top/err" >/dev/null

trap - EXIT HUP INT TERM
cleanup
printf 'identity Compose overlay selection tests passed\n'

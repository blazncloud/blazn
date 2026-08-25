#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$TEST_DIR/.." && pwd)
top=${TMPDIR:-/tmp}/blazn-control-plane-compose-test-$$
mkdir -p "$top/infra"
trap 'rm -rf -- "$top"' EXIT HUP INT TERM
printf 'services: {}\n' >"$top/infra/compose.yaml"
printf 'PUBLIC_URL=https://blazn.benpelo.com\n' >"$top/control-plane.env"

docker() {
  printf '%s\n' "$*" >"$top/docker.args"
  printf '%s\n' "${DOCKER_CONFIG:-}" >"$top/docker.config"
}
# shellcheck disable=SC1091
. "$ROOT_DIR/scripts/common.sh"

BLAZN_IDENTITY_ENABLED=false
BLAZN_DOCKER_CONFIG_ROOT=$top/docker-cli
export BLAZN_IDENTITY_ENABLED BLAZN_DOCKER_CONFIG_ROOT
control_plane_compose "$top/infra" "$top/control-plane.env" ps api
grep -Fx "compose -f $top/infra/compose.yaml --env-file $top/control-plane.env ps api" "$top/docker.args" >/dev/null
grep -Fx "$top/docker-cli" "$top/docker.config" >/dev/null

unset BLAZN_DOCKER_CONFIG_ROOT
control_plane_compose "$top/infra" "$top/control-plane.env" ps api
grep -Fx /etc/blazn/docker-cli "$top/docker.config" >/dev/null

trap - EXIT HUP INT TERM
rm -rf -- "$top"
printf 'control-plane Compose Docker configuration tests passed\n'

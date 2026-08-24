#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "control-plane startup must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "control-plane startup must run through with-control-plane-lock.sh"
require_command docker
export DOCKER_CONFIG="${BLAZN_DOCKER_CONFIG_ROOT:-/etc/blazn/docker-cli}"
ENV_FILE=${BLAZN_CONTROL_PLANE_ENV_FILE:-/etc/blazn/control-plane/control-plane.env}
assert_regular_file_owned_mode "$ENV_FILE" 0 600

build_mode=${BLAZN_CONTROL_API_BUILD_MODE:-local}
case $build_mode in
  local) "$SCRIPT_DIR/build-control-api.sh" ;;
  prebuilt) validate_control_api_build "$ROOT_DIR" ;;
  *) die "BLAZN_CONTROL_API_BUILD_MODE must be local or prebuilt" ;;
esac
load_control_api_image "$ROOT_DIR"
"$SCRIPT_DIR/preflight.sh" --deploy
docker compose -f "$ROOT_DIR/compose.yaml" --env-file "$ENV_FILE" up --detach --wait --remove-orphans
verify_control_api_containers "$ROOT_DIR" "$ENV_FILE"

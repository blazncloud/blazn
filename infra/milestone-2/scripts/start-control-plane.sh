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
broker_mode=$(sed -n 's/^BLAZN_NODE_BROKER_LOOPBACK=//p' "$ENV_FILE"); [ -n "$broker_mode" ] || broker_mode=disabled; case "$broker_mode" in enabled|disabled) ;; *) die "Node broker loopback binding must be enabled or disabled without duplicates" ;; esac
compose(){ if [ "$broker_mode" = enabled ]; then control_plane_compose "$ROOT_DIR" "$ENV_FILE" --profile node-broker "$@"; else control_plane_compose "$ROOT_DIR" "$ENV_FILE" "$@"; fi; }

build_mode=${BLAZN_CONTROL_API_BUILD_MODE:-local}
case $build_mode in
  local) "$SCRIPT_DIR/build-control-api.sh" ;;
  prebuilt) validate_control_api_build "$ROOT_DIR" ;;
  *) die "BLAZN_CONTROL_API_BUILD_MODE must be local or prebuilt" ;;
esac
load_control_api_image "$ROOT_DIR"
if identity_overlay_enabled; then
  "$SCRIPT_DIR/prepare-identity-runtime-secrets.sh"
  # File-backed Compose secrets are inode-bound. Recreate the only consumer so
  # an atomic credential rotation cannot restart a container on retired bytes.
  compose create --no-deps --force-recreate api
fi
"$SCRIPT_DIR/preflight.sh" --deploy
compose up --detach --wait --remove-orphans
verify_control_api_containers "$ROOT_DIR" "$ENV_FILE"
verify_node_prerequisite_containers "$ROOT_DIR" "$ENV_FILE"
verify_node_plan_container "$ROOT_DIR" "$ENV_FILE"
if [ "$broker_mode" = enabled ]; then container=$(compose ps -q node-broker); if [ -z "$container" ]; then die "Node broker sidecar has no container"; fi; if [ "$(docker inspect --format '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container")" != "running healthy" ]; then die "Node broker sidecar is not running and healthy"; fi; fi

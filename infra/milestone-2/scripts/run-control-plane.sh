#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "control-plane supervisor must run as root"
require_command docker
export DOCKER_CONFIG="${BLAZN_DOCKER_CONFIG_ROOT:-/etc/blazn/docker-cli}"
ENV_FILE=${BLAZN_CONTROL_PLANE_ENV_FILE:-/etc/blazn/control-plane/control-plane.env}
assert_regular_file_owned_mode "$ENV_FILE" 0 600
broker_mode=$(sed -n 's/^BLAZN_NODE_BROKER_LOOPBACK=//p' "$ENV_FILE"); [ -n "$broker_mode" ] || broker_mode=disabled; case "$broker_mode" in enabled|disabled) ;; *) die "Node broker loopback binding must be enabled or disabled without duplicates" ;; esac
load_control_api_image "$ROOT_DIR"

compose() {
  if [ "$broker_mode" = enabled ]; then docker compose -f "$ROOT_DIR/compose.yaml" --env-file "$ENV_FILE" --profile node-broker "$@"; else docker compose -f "$ROOT_DIR/compose.yaml" --env-file "$ENV_FILE" "$@"; fi
}

trap 'exit 0' HUP INT TERM

while :; do
  for service in postgres object api; do
    container=$(compose ps -q "$service")
    [ -n "$container" ] || die "required service has no container: $service"
    state=$(docker inspect --format '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container")
    [ "$state" = "running healthy" ] || die "required service is not running and healthy: $service ($state)"
  done
  verify_control_api_containers "$ROOT_DIR" "$ENV_FILE"
  verify_node_prerequisite_containers "$ROOT_DIR" "$ENV_FILE"
  verify_node_plan_container "$ROOT_DIR" "$ENV_FILE"
  if [ "$broker_mode" = enabled ]; then container=$(compose ps -q node-broker); if [ -z "$container" ]; then die "Node broker sidecar has no container"; fi; if [ "$(docker inspect --format '{{.State.Status}}' "$container")" != running ]; then die "Node broker sidecar is not running"; fi; fi
  sleep 5
done

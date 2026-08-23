#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "control-plane stop must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "control-plane stop must run through with-control-plane-lock.sh"
require_command docker
export DOCKER_CONFIG="${BLAZN_DOCKER_CONFIG_ROOT:-/etc/blazn/docker-cli}"
ENV_FILE=${BLAZN_CONTROL_PLANE_ENV_FILE:-/etc/blazn/control-plane/control-plane.env}
assert_regular_file_owned_mode "$ENV_FILE" 0 600
broker_mode=$(sed -n 's/^BLAZN_NODE_BROKER_LOOPBACK=//p' "$ENV_FILE"); [ -n "$broker_mode" ] || broker_mode=disabled; case "$broker_mode" in enabled|disabled) ;; *) die "Node broker loopback binding must be enabled or disabled without duplicates" ;; esac
load_control_api_image "$ROOT_DIR"
if [ "$broker_mode" = enabled ]; then control_plane_compose "$ROOT_DIR" "$ENV_FILE" --profile node-broker stop; else control_plane_compose "$ROOT_DIR" "$ENV_FILE" stop; fi

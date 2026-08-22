#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "control API build must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "control API build must run through with-control-plane-lock.sh"
require_command docker
require_command jq
require_command sha256sum
export DOCKER_CONFIG="${BLAZN_DOCKER_CONFIG_ROOT:-/etc/blazn/docker-cli}"
ENV_FILE=${BLAZN_CONTROL_PLANE_ENV_FILE:-/etc/blazn/control-plane/control-plane.env}
assert_regular_file_owned_mode "$ENV_FILE" 0 600

docker compose -f "$ROOT_DIR/compose.yaml" --env-file "$ENV_FILE" build api
source_digest=sha256:$(control_api_source_digest "$ROOT_DIR")
image_id=$(docker image inspect blazn-control-api:managed --format '{{.Id}}') || die "managed control API image build did not produce an image"
case "$image_id" in sha256:????????????????????????????????????????????????????????????????) ;; *) die "managed control API image ID is invalid" ;; esac

receipt=${BLAZN_CONTROL_API_BUILD_RECEIPT:-/var/lib/blazn/ownership/control-api-build.json}
require_absolute_path BLAZN_CONTROL_API_BUILD_RECEIPT "$receipt"
assert_not_symlink_chain "$receipt"
mkdir -p -- "$(dirname -- "$receipt")"
assert_directory_owned_mode "$(dirname -- "$receipt")" 0 700
tmp=$receipt.tmp.$$
jq -cn --arg sourceDigest "$source_digest" --arg imageId "$image_id" --arg builtAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
  '{schemaVersion:"blazn.dev/control-api-build/v1",sourceDigest:$sourceDigest,image:"blazn-control-api:managed",imageId:$imageId,builtAt:$builtAt}' >"$tmp"
chmod 0600 "$tmp"
mv -- "$tmp" "$receipt"
validate_control_api_build "$ROOT_DIR"
printf 'built and recorded receipt-bound control API image %s\n' "$image_id"

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
export COMPOSE_BAKE=false
ENV_FILE=${BLAZN_CONTROL_PLANE_ENV_FILE:-/etc/blazn/control-plane/control-plane.env}
assert_regular_file_owned_mode "$ENV_FILE" 0 600

source_before=sha256:$(control_api_source_digest "$ROOT_DIR")
final_image=blazn-control-api:source-${source_before#sha256:}
candidate_image=blazn-control-api:candidate-$$
cleanup() {
  docker image rm "$candidate_image" >/dev/null 2>&1 || true
}
trap cleanup EXIT HUP INT TERM
CONTROL_API_IMAGE=$candidate_image docker compose -f "$ROOT_DIR/compose.yaml" --env-file "$ENV_FILE" build api
candidate_id=$(docker image inspect "$candidate_image" --format '{{.Id}}') || die "candidate control API build did not produce an image"
source_after=sha256:$(control_api_source_digest "$ROOT_DIR")
[ "$source_before" = "$source_after" ] || die "control API source changed during build; candidate and prior receipt were preserved"

if docker image inspect "$final_image" >/dev/null 2>&1; then
  existing_id=$(docker image inspect "$final_image" --format '{{.Id}}')
  [ "$existing_id" = "$candidate_id" ] || die "immutable source-digest image tag already resolves to different content"
else
  docker image tag "$candidate_image" "$final_image"
fi
image_id=$(docker image inspect "$final_image" --format '{{.Id}}') || die "source-digest control API image is unavailable"
[ "$image_id" = "$candidate_id" ] || die "source-digest control API tag changed during promotion"
case "$image_id" in sha256:????????????????????????????????????????????????????????????????) ;; *) die "managed control API image ID is invalid" ;; esac
cleanup
trap - EXIT HUP INT TERM

receipt=${BLAZN_CONTROL_API_BUILD_RECEIPT:-/var/lib/blazn/ownership/control-api-build.json}
require_absolute_path BLAZN_CONTROL_API_BUILD_RECEIPT "$receipt"
assert_not_symlink_chain "$receipt"
mkdir -p -- "$(dirname -- "$receipt")"
assert_directory_owned_mode "$(dirname -- "$receipt")" 0 700
tmp=$receipt.tmp.$$
jq -cn --arg sourceDigest "$source_before" --arg image "$final_image" --arg imageId "$image_id" --arg builtAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
  '{schemaVersion:"blazn.dev/control-api-build/v1",sourceDigest:$sourceDigest,image:$image,imageId:$imageId,builtAt:$builtAt}' >"$tmp"
chmod 0600 "$tmp"
mv -- "$tmp" "$receipt"
validate_control_api_build "$ROOT_DIR"
printf 'built and recorded receipt-bound control API image %s\n' "$image_id"

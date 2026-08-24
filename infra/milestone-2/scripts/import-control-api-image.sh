#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "control API image import must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] || die "control API image import must run through with-control-plane-lock.sh"
[ "$#" -eq 2 ] || die "usage: import-control-api-image.sh IMAGE_ARCHIVE EXPECTED_ARCHIVE_SHA256"
archive=$1
expected_archive=$2
require_absolute_path IMAGE_ARCHIVE "$archive"
assert_not_symlink_chain "$archive"
assert_regular_file_owned_mode "$archive" 0 600
case $expected_archive in sha256:????????????????????????????????????????????????????????????????) ;; *) die "expected image archive digest is invalid" ;; esac
require_command docker
require_command jq
require_command sha256sum
require_command tar

actual_archive=sha256:$(sha256_file "$archive")
[ "$actual_archive" = "$expected_archive" ] || die "control API image archive digest mismatch"
source_digest=sha256:$(control_api_source_digest "$ROOT_DIR")
expected_image=blazn-control-api:source-${source_digest#sha256:}

manifest=$(mktemp /tmp/blazn-image-manifest.XXXXXX)
config=$(mktemp /tmp/blazn-image-config.XXXXXX)
cleanup() { unlink "$manifest" "$config" 2>/dev/null || true; }
trap cleanup EXIT HUP INT TERM
tar -xOf "$archive" manifest.json >"$manifest"
jq -e --arg image "$expected_image" 'length == 1 and .[0].RepoTags == [$image] and (. [0].Layers | length > 0)' "$manifest" >/dev/null || die "control API image archive manifest is invalid"
config_path=$(jq -er '.[0].Config' "$manifest")
case $config_path in ''|/*|*..*) die "control API image config path is invalid" ;; esac
tar -xOf "$archive" "$config_path" >"$config"
config_digest=$(sha256_file "$config")
case $config_path in
  "$config_digest.json"|"blobs/sha256/$config_digest") ;;
  *) die "control API image config digest is invalid" ;;
esac
archive_image_id=sha256:$config_digest
jq -e '
  .config.User == "node" and .config.WorkingDir == "/app" and
  .config.Cmd == ["node","dist/server.js"] and
  (.config.Env | index("NODE_ENV=production")) != null and
  (.config.ExposedPorts["8080/tcp"] == {}) and
  (.rootfs.diff_ids | length > 0)
' "$config" >/dev/null || die "control API image runtime configuration is invalid"

existing_id=$(docker image inspect "$expected_image" --format '{{.Id}}' 2>/dev/null || true)
if [ -n "$existing_id" ] && [ "$existing_id" != "$archive_image_id" ]; then
  die "immutable source-digest image tag already resolves to different content"
fi
docker load --input "$archive" >/dev/null
image_id=$(docker image inspect "$expected_image" --format '{{.Id}}') || die "imported control API image is unavailable"
[ "$image_id" = "$archive_image_id" ] || die "imported control API image ID differs from the verified archive"

receipt=${BLAZN_CONTROL_API_BUILD_RECEIPT:-/var/lib/blazn/ownership/control-api-build.json}
require_absolute_path BLAZN_CONTROL_API_BUILD_RECEIPT "$receipt"
assert_not_symlink_chain "$receipt"
mkdir -p -- "$(dirname -- "$receipt")"
assert_directory_owned_mode "$(dirname -- "$receipt")" 0 700
tmp=$receipt.tmp.$$
jq -cn --arg sourceDigest "$source_digest" --arg image "$expected_image" --arg imageId "$image_id" \
  --arg archiveDigest "$actual_archive" --arg importedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
  '{schemaVersion:"blazn.dev/control-api-build/v1",sourceDigest:$sourceDigest,image:$image,imageId:$imageId,builtAt:$importedAt,archiveDigest:$archiveDigest,buildMode:"prebuilt"}' >"$tmp"
chmod 0600 "$tmp"
mv -- "$tmp" "$receipt"
validate_control_api_build "$ROOT_DIR"
trap - EXIT HUP INT TERM
cleanup
printf 'imported and recorded receipt-bound control API image %s\n' "$image_id"

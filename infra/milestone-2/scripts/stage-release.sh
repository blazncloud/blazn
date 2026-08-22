#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "release staging must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] && [ -n "${BLAZN_CORRELATION_ID:-}" ] || die "release staging must run through with-control-plane-lock.sh"
[ "$#" -eq 2 ] || die "usage: stage-release.sh SOURCE_CHECKOUT EXPECTED_COMMIT"
source_checkout=$1
expected_commit=$2
[ "${#expected_commit}" -eq 40 ] || die "expected release commit must be 40 lowercase hexadecimal characters"
case "$expected_commit" in *[!a-f0-9]*) die "expected release commit must be 40 lowercase hexadecimal characters" ;; esac
require_absolute_path SOURCE_CHECKOUT "$source_checkout"
assert_not_symlink_chain "$source_checkout"
require_command git
require_command tar
require_command jq
require_command sha256sum
[ "$(git -C "$source_checkout" rev-parse HEAD)" = "$expected_commit" ] || die "source checkout HEAD differs from expected release commit"
[ "$(git -C "$source_checkout" rev-parse "$expected_commit^{commit}")" = "$expected_commit" ] || die "expected release commit is unavailable"
tree=$(git -C "$source_checkout" rev-parse "$expected_commit^{tree}")

release_root=${BLAZN_RELEASE_ROOT:-/opt/blazn-releases}
receipt_root=${BLAZN_RELEASE_RECEIPT_ROOT:-/var/lib/blazn/ownership/releases}
require_absolute_path BLAZN_RELEASE_ROOT "$release_root"
require_absolute_path BLAZN_RELEASE_RECEIPT_ROOT "$receipt_root"
assert_not_symlink_chain "$release_root"
assert_not_symlink_chain "$receipt_root"
umask 077
mkdir -p -- "$release_root" "$receipt_root"
chmod 0755 "$release_root"
chmod 0700 "$receipt_root"
final=$release_root/$expected_commit
receipt=$receipt_root/$expected_commit.json
if [ -e "$final" ] || [ -e "$receipt" ]; then
  if [ -d "$final" ] && [ ! -e "$receipt" ] && [ -f "$final/.blazn-release.json" ]; then
    recovery=$receipt.recovery.$$
    cp -- "$final/.blazn-release.json" "$recovery"
    chmod 0600 "$recovery"
    mv -- "$recovery" "$receipt"
  fi
  [ -d "$final" ] && [ -f "$receipt" ] || die "partial versioned release state exists"
  verify_versioned_release "$final" "$receipt"
  printf 'versioned release is already staged\n'
  exit 0
fi

stage=$release_root/.staging-$expected_commit-$$
archive=$release_root/.archive-$expected_commit-$$.tar
manifest_tmp=$release_root/.manifest-$expected_commit-$$
[ ! -e "$stage" ] && [ ! -e "$archive" ] && [ ! -e "$manifest_tmp" ] || die "release staging path collision"
cleanup() {
  [ ! -f "$archive" ] || unlink "$archive"
  [ ! -f "$manifest_tmp" ] || unlink "$manifest_tmp"
  if [ -d "$stage" ]; then
    find "$stage" -xdev -type f -delete
    find "$stage" -xdev -depth -type d -empty -delete
  fi
}
trap cleanup EXIT HUP INT TERM
mkdir "$stage"
git -C "$source_checkout" archive --format=tar --output="$archive" "$expected_commit"
tar -xf "$archive" -C "$stage" --no-same-owner --no-same-permissions
unlink "$archive"
if find "$stage" -xdev \( -type l -o ! -type d ! -type f \) -print | grep . >/dev/null; then
  die "release archive contains a link or special file"
fi
(
  cd "$stage"
  find . -type f -print0 | LC_ALL=C sort -z | xargs -0 sha256sum
) >"$manifest_tmp"
mv -- "$manifest_tmp" "$stage/.blazn-release-files.sha256"
control_api_source=sha256:$(control_api_source_digest "$stage/infra/milestone-2")
control_plane_config=sha256:$(control_plane_config_digest "$stage/infra/milestone-2")
unit_digest=sha256:$(sha256_file "$stage/infra/milestone-2/systemd/blazn-control-plane.service")
manifest_digest=sha256:$(sha256_file "$stage/.blazn-release-files.sha256")
release_digest=sha256:$(printf '%s\n%s\n%s\n%s\n' "$expected_commit" "$tree" "$manifest_digest" "$control_plane_config" | sha256sum | awk '{print $1}')
receipt_tmp=$receipt.tmp.$$
jq -cn --arg id "$expected_commit" --arg commit "$expected_commit" --arg tree "$tree" --arg path "$final" \
  --arg manifestDigest "$manifest_digest" --arg releaseDigest "$release_digest" --arg controlApiSourceDigest "$control_api_source" \
  --arg controlPlaneConfigDigest "$control_plane_config" --arg systemdUnitDigest "$unit_digest" \
  --arg stagedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" --argjson fencingToken "$BLAZN_FENCING_TOKEN" --arg correlationId "$BLAZN_CORRELATION_ID" \
  '{schemaVersion:"blazn.dev/release/v1",id:$id,commit:$commit,tree:$tree,path:$path,manifestDigest:$manifestDigest,releaseDigest:$releaseDigest,controlApiSourceDigest:$controlApiSourceDigest,controlPlaneConfigDigest:$controlPlaneConfigDigest,systemdUnitDigest:$systemdUnitDigest,stagedAt:$stagedAt,fencingToken:$fencingToken,correlationId:$correlationId}' >"$receipt_tmp"
chmod 0600 "$receipt_tmp"
cp -- "$receipt_tmp" "$stage/.blazn-release.json"
chmod 0444 "$stage/.blazn-release.json"
find "$stage" -xdev -type f -perm /111 -exec chmod 0555 {} +
find "$stage" -xdev -type f ! -perm /111 -exec chmod 0444 {} +
find "$stage" -xdev -type d -exec chmod 0555 {} +
chown -R 0:0 "$stage"
mv -- "$stage" "$final"
mv -- "$receipt_tmp" "$receipt"
verify_versioned_release "$final" "$receipt"
trap - EXIT HUP INT TERM
printf 'staged immutable release %s\n' "$expected_commit"

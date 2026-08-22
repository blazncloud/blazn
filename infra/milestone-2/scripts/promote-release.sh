#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "release promotion must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] && [ -n "${BLAZN_CORRELATION_ID:-}" ] || die "release promotion must run through with-control-plane-lock.sh"
[ "$#" -eq 1 ] || die "usage: promote-release.sh RELEASE_ID"
release_id=$1
case $release_id in ''|*[!a-zA-Z0-9._-]*) die "release ID contains unsupported characters" ;; esac
require_command jq
require_command sha256sum
require_command systemctl
require_command findmnt
require_command install

release_root=${BLAZN_RELEASE_ROOT:-/opt/blazn-releases}
receipt_root=${BLAZN_RELEASE_RECEIPT_ROOT:-/var/lib/blazn/ownership/releases}
active=${BLAZN_ACTIVE_RELEASE_PATH:-/opt/blazn}
active_receipt=${BLAZN_ACTIVE_RELEASE_RECEIPT:-/var/lib/blazn/ownership/active-release.json}
intent=${BLAZN_RELEASE_PROMOTION_INTENT:-/var/lib/blazn/ownership/release-promotion-intent.json}
installed_unit=${BLAZN_SYSTEMD_UNIT_PATH:-/etc/systemd/system/blazn-control-plane.service}
candidate=$release_root/$release_id
candidate_receipt=$receipt_root/$release_id.json
for path in "$release_root" "$receipt_root" "$active" "$active_receipt" "$intent"; do assert_not_symlink_chain "$(dirname -- "$path")"; done
verify_managed_release "$candidate" "$candidate_receipt"

if [ -L "$active" ] && [ "$(readlink -f "$active")" = "$candidate" ] && [ -f "$active_receipt" ] && [ ! -e "$intent" ]; then
  assert_regular_file_owned_mode "$active_receipt" 0 600
  jq -e --arg activeId "$release_id" --arg releaseDigest "$(jq -er .releaseDigest "$candidate_receipt")" \
    '.schemaVersion == "blazn.dev/active-release/v1" and .activeId == $activeId and .releaseDigest == $releaseDigest' "$active_receipt" >/dev/null || die "active release receipt disagrees with the active link"
  cmp -s "$candidate/infra/milestone-2/systemd/blazn-control-plane.service" "$installed_unit" || die "installed systemd unit differs from the active release"
  printf 'release %s is already active\n' "$release_id"
  exit 0
fi

state=$(systemctl is-active blazn-control-plane.service 2>/dev/null || true)
case $state in inactive|failed|unknown|'') ;; *) die "control-plane service must be fully stopped before release promotion (state=$state)" ;; esac
[ "$(stat -c '%d' "$release_root")" = "$(stat -c '%d' "$(dirname -- "$active")")" ] || die "active and versioned releases must share a filesystem for atomic promotion"
require_absolute_path BLAZN_SYSTEMD_UNIT_PATH "$installed_unit"
assert_not_symlink_chain "$installed_unit"
if [ -e "$installed_unit" ]; then assert_regular_file_owned_mode "$installed_unit" 0 644; fi

if [ -e "$intent" ]; then
  assert_regular_file_owned_mode "$intent" 0 600
  jq -e --arg candidate "$release_id" --arg correlation "$BLAZN_CORRELATION_ID" \
    '.schemaVersion == "blazn.dev/release-promotion-intent/v1" and .candidateId == $candidate and .correlationId == $correlation' \
    "$intent" >/dev/null || die "an incompatible release promotion intent requires reconciliation"
  previous_id=$(jq -r '.previousId // empty' "$intent")
  adopt_path=$(jq -r '.adoptPath // empty' "$intent")
else
  previous_id=
  adopt_path=
  if [ -L "$active" ]; then
    previous_path=$(readlink -f "$active")
    case "$previous_path" in "$release_root"/*) ;; *) die "active release link escapes the release root" ;; esac
    previous_id=$(basename -- "$previous_path")
    [ "$previous_path" = "$release_root/$previous_id" ] || die "active release must be a direct child of the release root"
    verify_managed_release "$previous_path" "$receipt_root/$previous_id.json"
  elif [ -d "$active" ]; then
    findmnt -rn --mountpoint "$active" >/dev/null 2>&1 && die "legacy active release must not be a mountpoint"
    if find "$active" -xdev \( -type l -o ! -type d ! -type f \) -print | grep . >/dev/null; then die "legacy active release contains a link or special file"; fi
    legacy_manifest_tmp=$receipt_root/.legacy-manifest-$$
    (cd "$active"; find . -xdev -type f -print0 | LC_ALL=C sort -z | xargs -0 sha256sum) >"$legacy_manifest_tmp"
    chmod 0600 "$legacy_manifest_tmp"
    legacy_digest=$(sha256_file "$legacy_manifest_tmp")
    previous_id=legacy-$(printf '%.16s' "$legacy_digest")
    adopt_path=$release_root/$previous_id
    legacy_manifest=$receipt_root/$previous_id.sha256
    if [ -e "$legacy_manifest" ]; then
      assert_regular_file_owned_mode "$legacy_manifest" 0 600
      cmp -s "$legacy_manifest_tmp" "$legacy_manifest" || die "legacy release manifest collision"
      unlink "$legacy_manifest_tmp"
    else
      mv -- "$legacy_manifest_tmp" "$legacy_manifest"
    fi
    if [ -e "$receipt_root/$previous_id.json" ]; then
      assert_regular_file_owned_mode "$receipt_root/$previous_id.json" 0 600
      jq -e --arg id "$previous_id" --arg path "$adopt_path" --arg manifestPath "$legacy_manifest" --arg manifestDigest "sha256:$legacy_digest" \
        '.schemaVersion=="blazn.dev/legacy-release/v1" and .id==$id and .path==$path and .manifestPath==$manifestPath and .manifestDigest==$manifestDigest and .releaseDigest==$manifestDigest' \
        "$receipt_root/$previous_id.json" >/dev/null || die "legacy release receipt collision"
    else
      legacy_receipt_tmp=$receipt_root/$previous_id.json.tmp.$$
      jq -cn --arg id "$previous_id" --arg path "$adopt_path" --arg manifestPath "$legacy_manifest" \
        --arg manifestDigest "sha256:$legacy_digest" --arg releaseDigest "sha256:$legacy_digest" --arg adoptedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
        '{schemaVersion:"blazn.dev/legacy-release/v1",id:$id,path:$path,manifestPath:$manifestPath,manifestDigest:$manifestDigest,releaseDigest:$releaseDigest,adoptedAt:$adoptedAt}' >"$legacy_receipt_tmp"
      chmod 0600 "$legacy_receipt_tmp"
      mv -- "$legacy_receipt_tmp" "$receipt_root/$previous_id.json"
    fi
    (cd "$active"; sha256sum -c "$legacy_manifest" >/dev/null) || die "legacy active release changed while preparing adoption"
  elif [ -e "$active" ]; then
    die "active release path is neither a directory nor a managed link"
  fi
  intent_tmp=$intent.tmp.$$
  jq -cn --arg candidateId "$release_id" --arg previousId "$previous_id" --arg adoptPath "$adopt_path" \
    --arg correlationId "$BLAZN_CORRELATION_ID" --argjson fencingToken "$BLAZN_FENCING_TOKEN" \
    '{schemaVersion:"blazn.dev/release-promotion-intent/v1",candidateId:$candidateId,previousId:(if $previousId=="" then null else $previousId end),adoptPath:(if $adoptPath=="" then null else $adoptPath end),correlationId:$correlationId,fencingToken:$fencingToken}' >"$intent_tmp"
  chmod 0600 "$intent_tmp"
  mv -- "$intent_tmp" "$intent"
fi

if [ -n "$adopt_path" ] && [ -d "$active" ] && [ ! -e "$adopt_path" ]; then
  mv -- "$active" "$adopt_path"
  chown -R 0:0 "$adopt_path"
  find "$adopt_path" -xdev -type f -perm /111 -exec chmod 0555 {} +
  find "$adopt_path" -xdev -type f ! -perm /111 -exec chmod 0444 {} +
  find "$adopt_path" -xdev -type d -exec chmod 0555 {} +
  verify_legacy_release "$adopt_path" "$receipt_root/$previous_id.json"
fi
if [ -n "$adopt_path" ] && [ -d "$adopt_path" ]; then
  verify_legacy_release "$adopt_path" "$receipt_root/$previous_id.json"
fi
[ "${BLAZN_PROMOTION_FAILPOINT:-}" != after-adopt ] || die "injected promotion failure after legacy adoption"

active_target=$(readlink -f "$active" 2>/dev/null || true)
if [ "$active_target" != "$candidate" ]; then
  [ ! -e "$active" ] || [ -L "$active" ] || die "promotion intent did not reconcile the active path"
  link_tmp=$(dirname -- "$active")/.blazn-active-$$
  [ ! -e "$link_tmp" ] || die "active release temporary link exists"
  ln -s "$candidate" "$link_tmp"
  mv -Tf -- "$link_tmp" "$active"
fi
[ "${BLAZN_PROMOTION_FAILPOINT:-}" != after-link ] || die "injected promotion failure after active-link swap"

unit_source=$candidate/infra/milestone-2/systemd/blazn-control-plane.service
unit_tmp=$(dirname -- "$installed_unit")/.blazn-control-plane.service.$$
install -o root -g root -m 0644 "$unit_source" "$unit_tmp"
mv -- "$unit_tmp" "$installed_unit"
cmp -s "$unit_source" "$installed_unit" || die "installed systemd unit differs after release promotion"
systemctl daemon-reload

release_digest=$(jq -er .releaseDigest "$candidate_receipt")
active_tmp=$active_receipt.tmp.$$
jq -cn --arg activeId "$release_id" --arg previousId "$previous_id" --arg releaseDigest "$release_digest" \
  --arg correlationId "$BLAZN_CORRELATION_ID" --argjson fencingToken "$BLAZN_FENCING_TOKEN" --arg promotedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
  '{schemaVersion:"blazn.dev/active-release/v1",activeId:$activeId,previousId:(if $previousId=="" then null else $previousId end),releaseDigest:$releaseDigest,correlationId:$correlationId,fencingToken:$fencingToken,promotedAt:$promotedAt}' >"$active_tmp"
chmod 0600 "$active_tmp"
mv -- "$active_tmp" "$active_receipt"
unlink "$intent"
verify_managed_release "$candidate" "$candidate_receipt"
[ "$(readlink -f "$active")" = "$candidate" ] || die "active release link changed after promotion"
printf 'promoted release %s; previous release preserved as %s\n' "$release_id" "${previous_id:-none}"

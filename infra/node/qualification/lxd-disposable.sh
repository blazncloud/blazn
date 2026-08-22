#!/usr/bin/env bash
# shellcheck source-path=SCRIPTDIR

set -o errexit
set -o nounset
set -o pipefail
script_dir=$(unset CDPATH; cd -- "$(dirname -- "$0")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"

action=${1:-plan}
qual_require_target
[ "${BLAZN_QUALIFICATION_PROFILE:-}" = lxd-ubuntu-26.04 ] || qual_die 'LXD lifecycle requires profile lxd-ubuntu-26.04'
qual_guest_name_matches_correlation

guest=$BLAZN_QUALIFICATION_TARGET
image_fingerprint=${BLAZN_QUALIFICATION_LXD_IMAGE_FINGERPRINT:-}
[[ "$image_fingerprint" =~ ^[0-9a-f]{64}$ ]] || qual_die 'BLAZN_QUALIFICATION_LXD_IMAGE_FINGERPRINT must pin the reviewed Ubuntu 26.04 image'
image="images:${image_fingerprint}"
case "$action" in plan|create) qual_validate_lxd_limits ;; esac

if [ "$action" = plan ]; then
  printf '%s\n' "target=${guest}" "imageFingerprint=${image_fingerprint}" "cpu=${BLAZN_QUALIFICATION_LXD_CPU}" "memory=${BLAZN_QUALIFICATION_LXD_MEMORY}" "rootDisk=${BLAZN_QUALIFICATION_LXD_ROOT_DISK}" "processes=${BLAZN_QUALIFICATION_LXD_PROCESSES}" 'mutations=create|snapshot|restore|delete (one explicit action per approval)'
  exit 0
fi

qual_require_command lxc
qual_require_command jq

guest_exists() { lxc info "$guest" >/dev/null 2>&1; }
guest_owned() {
  [ "$(lxc config get "$guest" user.blazn.qualification 2>/dev/null || true)" = "$BLAZN_QUALIFICATION_CORRELATION_ID" ] || qual_die 'guest is not owned by this qualification correlation'
}

do_create() {
  guest_exists && qual_die 'refusing to replace an existing LXD instance'
  lxc launch "$image" "$guest" \
    -c limits.cpu="$BLAZN_QUALIFICATION_LXD_CPU" \
    -c limits.memory="$BLAZN_QUALIFICATION_LXD_MEMORY" \
    -c limits.processes="$BLAZN_QUALIFICATION_LXD_PROCESSES" \
    -d root,size="$BLAZN_QUALIFICATION_LXD_ROOT_DISK" \
    -c security.privileged=false \
    -c user.blazn.qualification="$BLAZN_QUALIFICATION_CORRELATION_ID" \
    -c user.blazn.purpose=node-platform-qualification >/dev/null
  jq -n --arg digest "$BLAZN_QUALIFICATION_ACCEPTED_INPUT_DIGEST" --arg target "$guest" --arg image "$image_fingerprint" --arg cpu "$BLAZN_QUALIFICATION_LXD_CPU" --arg memory "$BLAZN_QUALIFICATION_LXD_MEMORY" --arg rootDisk "$BLAZN_QUALIFICATION_LXD_ROOT_DISK" --arg processes "$BLAZN_QUALIFICATION_LXD_PROCESSES" \
    '{schemaVersion:1,status:"passed",qualificationApprovalInputDigest:$digest,target:$target,imageFingerprintDigest:("sha256:"+$image),limits:{cpu:$cpu,memory:$memory,rootDisk:$rootDisk,processes:$processes}}'
}

do_delete() {
  guest_exists || qual_die 'qualification guest does not exist'
  guest_owned
  lxc delete --force "$guest"
}

do_snapshot() {
  snapshot=${BLAZN_QUALIFICATION_SNAPSHOT:-}
  [[ "$snapshot" =~ ^checkpoint-[a-z0-9][a-z0-9-]{1,47}$ ]] || qual_die 'snapshot must be a bounded DNS-safe checkpoint name'
  guest_exists || qual_die 'qualification guest does not exist'
  guest_owned
  lxc snapshot "$guest" "$snapshot"
}

do_restore() {
  snapshot=${BLAZN_QUALIFICATION_SNAPSHOT:-}
  [[ "$snapshot" =~ ^checkpoint-[a-z0-9][a-z0-9-]{1,47}$ ]] || qual_die 'snapshot must be a bounded DNS-safe checkpoint name'
  guest_exists || qual_die 'qualification guest does not exist'
  guest_owned
  lxc restore "$guest" "$snapshot"
}

case "$action" in
  inspect)
    guest_exists || qual_die 'qualification guest does not exist'
    guest_owned
    lxc list "$guest" --format json | jq 'map({name,status,type,architecture,location})'
    ;;
  create|snapshot|restore|delete)
    qual_require_approval "lxd-${action}"
    qual_with_lock "do_${action}"
    ;;
  *) qual_die 'usage: lxd-disposable.sh plan|inspect|create|snapshot|restore|delete' ;;
esac

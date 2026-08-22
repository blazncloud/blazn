#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "POC CLI user management must run as root"
if [ -z "${BLAZN_FENCING_TOKEN:-}" ] || [ -z "${BLAZN_CORRELATION_ID:-}" ]; then die "POC CLI user management must run through with-control-plane-lock.sh"; fi
[ "$#" -eq 1 ] || die "usage: manage-poc-cli-users.sh provision|cleanup"
action=$1
case $action in provision|cleanup) ;; *) die "unsupported POC CLI user action" ;; esac
for command in getent id useradd userdel groupdel jq findmnt; do require_command "$command"; done
nologin=$(command -v nologin || true)
[ -n "$nologin" ] || die "nologin shell is unavailable"

users_root=${BLAZN_POC_CLI_USERS_ROOT:-/var/lib/blazn-poc-users}
receipt=${BLAZN_POC_CLI_USERS_RECEIPT:-/var/lib/blazn/ownership/poc-cli-users.json}
intent=${BLAZN_POC_CLI_USERS_INTENT:-/var/lib/blazn/ownership/poc-cli-users-intent.json}
owner_name=${BLAZN_POC_OWNER_OS_USER:-blazn-poc-owner}
second_name=${BLAZN_POC_SECOND_OS_USER:-blazn-poc-second}
for account_name in "$owner_name" "$second_name"; do case $account_name in ''|*[!a-z0-9_-]*) die "POC CLI user names contain unsupported characters" ;; esac; done
[ "$owner_name" != "$second_name" ] || die "POC CLI users must be distinct"
owner_home=$users_root/owner
second_home=$users_root/second
for path in "$users_root" "$receipt" "$intent" "$owner_home" "$second_home"; do require_absolute_path POC_CLI_PATH "$path"; assert_not_symlink_chain "$path"; done

validate_account() {
  account_name=$1
  expected_home=$2
  passwd_record=$(getent passwd "$account_name") || die "receipted POC CLI user is absent: $account_name"
  IFS=: read -r actual_name _passwd uid gid _gecos actual_home actual_shell <<EOF
$passwd_record
EOF
  if [ "$actual_name" != "$account_name" ] || [ "$actual_home" != "$expected_home" ] || [ "$actual_shell" != "$nologin" ]; then die "POC CLI passwd record differs from its receipt plan"; fi
  if ! is_uint "$uid" || ! is_uint "$gid" || [ "$uid" -le 0 ] || [ "$gid" -le 0 ]; then die "POC CLI user IDs are invalid"; fi
  [ "$(id -G "$account_name")" = "$gid" ] || die "POC CLI user has supplementary groups"
  assert_directory_owned_mode "$expected_home" "$uid" 700
  printf '%s:%s\n' "$uid" "$gid"
}

validate_receipt() {
  assert_regular_file_owned_mode "$receipt" 0 600
  owner_ids=$(validate_account "$owner_name" "$owner_home")
  second_ids=$(validate_account "$second_name" "$second_home")
  owner_uid=${owner_ids%%:*}; owner_gid=${owner_ids##*:}
  second_uid=${second_ids%%:*}; second_gid=${second_ids##*:}
  [ "$owner_uid" != "$second_uid" ] || die "POC CLI users share a UID"
  jq -e --arg ownerName "$owner_name" --arg ownerHome "$owner_home" --argjson ownerUid "$owner_uid" --argjson ownerGid "$owner_gid" \
    --arg secondName "$second_name" --arg secondHome "$second_home" --argjson secondUid "$second_uid" --argjson secondGid "$second_gid" \
    '.schemaVersion=="blazn.dev/poc-cli-users/v1" and .status=="active" and .owner=={name:$ownerName,uid:$ownerUid,gid:$ownerGid,home:$ownerHome} and .second=={name:$secondName,uid:$secondUid,gid:$secondGid,home:$secondHome}' "$receipt" >/dev/null || die "POC CLI user receipt differs from passwd state"
}

case $action in
  provision)
    if [ -e "$receipt" ]; then validate_receipt; printf 'POC CLI users are already provisioned\n'; exit 0; fi
    if [ -e "$intent" ]; then
      assert_regular_file_owned_mode "$intent" 0 600
      jq -e --arg owner "$owner_name" --arg second "$second_name" --arg ownerHome "$owner_home" --arg secondHome "$second_home" \
        '.schemaVersion=="blazn.dev/poc-cli-users-intent/v1" and .ownerName==$owner and .secondName==$second and .ownerHome==$ownerHome and .secondHome==$secondHome' "$intent" >/dev/null || die "POC CLI user intent differs from requested accounts"
    else
      getent passwd "$owner_name" >/dev/null 2>&1 && die "unreceipted owner qualification OS user already exists"
      getent passwd "$second_name" >/dev/null 2>&1 && die "unreceipted second qualification OS user already exists"
      [ ! -e "$users_root" ] || die "unreceipted POC CLI users root already exists"
      mkdir -p -- "$(dirname -- "$receipt")"
      chmod 0700 "$(dirname -- "$receipt")"
      intent_tmp=$intent.tmp.$$
      jq -cn --arg ownerName "$owner_name" --arg secondName "$second_name" --arg ownerHome "$owner_home" --arg secondHome "$second_home" --arg correlationId "$BLAZN_CORRELATION_ID" --argjson fencingToken "$BLAZN_FENCING_TOKEN" \
        '{schemaVersion:"blazn.dev/poc-cli-users-intent/v1",ownerName:$ownerName,secondName:$secondName,ownerHome:$ownerHome,secondHome:$secondHome,correlationId:$correlationId,fencingToken:$fencingToken}' >"$intent_tmp"
      chmod 0600 "$intent_tmp"
      mv -- "$intent_tmp" "$intent"
      mkdir "$users_root"
      chmod 0711 "$users_root"
    fi
    if [ ! -e "$users_root" ]; then mkdir "$users_root"; chmod 0711 "$users_root"; fi
    assert_directory_owned_mode "$users_root" 0 711
    empty_skel=$users_root/.empty-skel
    if [ ! -e "$empty_skel" ]; then mkdir "$empty_skel"; chmod 0755 "$empty_skel"; fi
    assert_directory_owned_mode "$empty_skel" 0 755
    for spec in "$owner_name:$owner_home" "$second_name:$second_home"; do
      account_name=${spec%%:*}; account_home=${spec#*:}
      if ! getent passwd "$account_name" >/dev/null 2>&1; then
        useradd --system --user-group --home-dir "$account_home" --create-home --skel "$empty_skel" --shell "$nologin" "$account_name"
      fi
      account_uid=$(id -u "$account_name"); account_gid=$(id -g "$account_name")
      chown "$account_uid:$account_gid" "$account_home"
      chmod 0700 "$account_home"
      ids=$(validate_account "$account_name" "$account_home")
      [ -n "$ids" ] || die "POC CLI account validation failed"
    done
    rmdir "$empty_skel"
    owner_ids=$(validate_account "$owner_name" "$owner_home"); second_ids=$(validate_account "$second_name" "$second_home")
    owner_uid=${owner_ids%%:*}; owner_gid=${owner_ids##*:}; second_uid=${second_ids%%:*}; second_gid=${second_ids##*:}
    receipt_tmp=$receipt.tmp.$$
    jq -cn --arg ownerName "$owner_name" --arg ownerHome "$owner_home" --argjson ownerUid "$owner_uid" --argjson ownerGid "$owner_gid" \
      --arg secondName "$second_name" --arg secondHome "$second_home" --argjson secondUid "$second_uid" --argjson secondGid "$second_gid" \
      --arg createdAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" --argjson fencingToken "$BLAZN_FENCING_TOKEN" --arg correlationId "$BLAZN_CORRELATION_ID" \
      '{schemaVersion:"blazn.dev/poc-cli-users/v1",status:"active",owner:{name:$ownerName,uid:$ownerUid,gid:$ownerGid,home:$ownerHome},second:{name:$secondName,uid:$secondUid,gid:$secondGid,home:$secondHome},createdAt:$createdAt,fencingToken:$fencingToken,correlationId:$correlationId}' >"$receipt_tmp"
    chmod 0600 "$receipt_tmp"
    mv -- "$receipt_tmp" "$receipt"
    unlink "$intent"
    validate_receipt
    printf 'POC CLI users provisioned with distinct passwd homes and no supplementary groups\n'
    ;;
  cleanup)
    if [ -e "$receipt" ] && jq -e '.schemaVersion=="blazn.dev/poc-cli-users/v1" and .status=="cleaned"' "$receipt" >/dev/null 2>&1; then
      getent passwd "$owner_name" >/dev/null 2>&1 && die "cleaned POC owner OS user still exists"
      getent passwd "$second_name" >/dev/null 2>&1 && die "cleaned POC second OS user still exists"
      [ ! -e "$users_root" ] || die "cleaned POC CLI users root still exists"
      if [ -e "$intent" ]; then
        assert_regular_file_owned_mode "$intent" 0 600
        jq -e '.schemaVersion=="blazn.dev/poc-cli-users-cleanup/v1" and ([.progress.owner[],.progress.second[]] | all)' "$intent" >/dev/null || die "cleaned POC CLI receipt has an incomplete cleanup intent"
        unlink "$intent"
      fi
      printf 'POC CLI users are already cleaned\n'
      exit 0
    fi
    assert_regular_file_owned_mode "$receipt" 0 600
    if [ ! -e "$intent" ]; then
      validate_receipt
      intent_tmp=$intent.tmp.$$
      receipt_digest=sha256:$(sha256_file "$receipt")
      jq -cn --arg receiptDigest "$receipt_digest" --argjson owner "$(jq -c .owner "$receipt")" --argjson second "$(jq -c .second "$receipt")" \
        --arg correlationId "$BLAZN_CORRELATION_ID" --argjson fencingToken "$BLAZN_FENCING_TOKEN" \
        '{schemaVersion:"blazn.dev/poc-cli-users-cleanup/v1",receiptDigest:$receiptDigest,owner:$owner,second:$second,correlationId:$correlationId,fencingToken:$fencingToken,progress:{owner:{user:false,group:false,home:false},second:{user:false,group:false,home:false}}}' >"$intent_tmp"
      chmod 0600 "$intent_tmp"
      mv -- "$intent_tmp" "$intent"
    fi
    assert_regular_file_owned_mode "$intent" 0 600
    jq -e --arg receiptDigest "sha256:$(sha256_file "$receipt")" --arg ownerName "$owner_name" --arg ownerHome "$owner_home" --arg secondName "$second_name" --arg secondHome "$second_home" \
      '.schemaVersion=="blazn.dev/poc-cli-users-cleanup/v1" and .receiptDigest==$receiptDigest and .owner.name==$ownerName and .owner.home==$ownerHome and .second.name==$secondName and .second.home==$secondHome' "$intent" >/dev/null || die "POC CLI cleanup intent differs from its active receipt"

    mark_progress() {
      progress_role=$1; progress_step=$2
      next=$intent.next.$$
      jq --arg role "$progress_role" --arg step "$progress_step" '.progress[$role][$step]=true' "$intent" >"$next"
      chmod 0600 "$next"
      mv -- "$next" "$intent"
      [ "${BLAZN_POC_CLI_CLEANUP_FAILPOINT:-}" != "after-$progress_role-$progress_step" ] || die "injected POC CLI cleanup failure after $progress_role $progress_step"
    }

    for role in second owner; do
      account_name=$(jq -er --arg role "$role" '.[$role].name' "$intent")
      account_home=$(jq -er --arg role "$role" '.[$role].home' "$intent")
      account_uid=$(jq -er --arg role "$role" '.[$role].uid' "$intent")
      account_gid=$(jq -er --arg role "$role" '.[$role].gid' "$intent")
      if [ "$(jq -r --arg role "$role" '.progress[$role].user' "$intent")" != true ]; then
        if getent passwd "$account_name" >/dev/null 2>&1; then
          ids=$(validate_account "$account_name" "$account_home")
          [ "$ids" = "$account_uid:$account_gid" ] || die "POC CLI user changed during cleanup"
          if command -v pgrep >/dev/null 2>&1 && pgrep -u "$account_uid" >/dev/null 2>&1; then die "POC CLI user still owns a running process: $account_name"; fi
          userdel "$account_name"
        fi
        getent passwd "$account_name" >/dev/null 2>&1 && die "POC CLI user remained after cleanup"
        mark_progress "$role" user
      fi
      if [ "$(jq -r --arg role "$role" '.progress[$role].group' "$intent")" != true ]; then
        if group_record=$(getent group "$account_name" 2>/dev/null); then
          IFS=: read -r group_name _group_password group_gid group_members <<EOF
$group_record
EOF
          [ "$group_name" = "$account_name" ] && [ "$group_gid" = "$account_gid" ] && [ -z "$group_members" ] || die "POC CLI primary group changed during cleanup"
          groupdel "$account_name"
        fi
        getent group "$account_name" >/dev/null 2>&1 && die "POC CLI group remained after cleanup"
        mark_progress "$role" group
      fi
      if [ "$(jq -r --arg role "$role" '.progress[$role].home' "$intent")" != true ]; then
        if [ -e "$account_home" ]; then
          assert_directory_owned_mode "$account_home" "$account_uid" 700
          findmnt -rn --mountpoint "$account_home" >/dev/null 2>&1 && die "POC CLI home is a mountpoint"
          if find "$account_home" -xdev \( -type l -o ! -type d ! -type f \) -print | grep . >/dev/null; then die "POC CLI home contains a link or special file"; fi
          find "$account_home" -xdev -type f -delete
          find "$account_home" -xdev -depth -type d -empty -delete
        fi
        [ ! -e "$account_home" ] || die "POC CLI home remained after cleanup"
        mark_progress "$role" home
      fi
    done
    if [ -e "$users_root" ]; then assert_directory_owned_mode "$users_root" 0 711; rmdir "$users_root"; fi
    receipt_tmp=$receipt.tmp.$$
    jq --arg cleanedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" '.status="cleaned" | .cleanedAt=$cleanedAt' "$receipt" >"$receipt_tmp"
    chmod 0600 "$receipt_tmp"
    mv -- "$receipt_tmp" "$receipt"
    [ "${BLAZN_POC_CLI_CLEANUP_FAILPOINT:-}" != after-receipt ] || die "injected POC CLI cleanup failure after receipt"
    unlink "$intent"
    printf 'receipt-owned POC CLI users and homes cleaned\n'
    ;;
esac

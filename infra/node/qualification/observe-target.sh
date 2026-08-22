#!/usr/bin/env bash
# shellcheck source-path=SCRIPTDIR

set -o errexit
set -o nounset
set -o pipefail
script_dir=$(unset CDPATH; cd -- "$(dirname -- "$0")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"

qual_require_target
qual_require_command jq
service=${BLAZN_QUALIFICATION_SERVICE_NAME:-blazn-node.service}
[ "$service" = blazn-node.service ] || qual_die 'only blazn-node.service may be observed'

if [ "$BLAZN_QUALIFICATION_PROFILE" = lxd-ubuntu-26.04 ]; then
  qual_guest_name_matches_correlation
  [ "$(lxc config get "$BLAZN_QUALIFICATION_TARGET" user.blazn.qualification 2>/dev/null || true)" = "$BLAZN_QUALIFICATION_CORRELATION_ID" ] || qual_die 'guest correlation marker differs'
  # shellcheck disable=SC2016
  os_id=$(lxc exec "$BLAZN_QUALIFICATION_TARGET" -- awk -F= '$1=="ID" {gsub(/"/,"",$2); print $2}' /etc/os-release)
  # shellcheck disable=SC2016
  os_version=$(lxc exec "$BLAZN_QUALIFICATION_TARGET" -- awk -F= '$1=="VERSION_ID" {gsub(/"/,"",$2); print $2}' /etc/os-release)
  [ "$os_id" = ubuntu ] && [ "$os_version" = 26.04 ] || qual_die "guest is not Ubuntu 26.04 (${os_id} ${os_version})"
  account_uid=$(lxc exec "$BLAZN_QUALIFICATION_TARGET" -- sh -c 'id -u blazn-node 2>/dev/null || printf absent')
  account_gid=$(lxc exec "$BLAZN_QUALIFICATION_TARGET" -- sh -c 'id -g blazn-node 2>/dev/null || printf absent')
  unit_user=$(lxc exec "$BLAZN_QUALIFICATION_TARGET" -- systemctl show "$service" --property=User --value 2>/dev/null || printf absent)
  main_pid=$(lxc exec "$BLAZN_QUALIFICATION_TARGET" -- systemctl show "$service" --property=MainPID --value 2>/dev/null || printf 0)
  process_uid=absent
  case "$main_pid" in ''|0|*[!0-9]*) ;; *) process_uid=$(lxc exec "$BLAZN_QUALIFICATION_TARGET" -- stat -c %u "/proc/${main_pid}") ;; esac
  observer=denied
  if [ "$account_uid" != absent ] && lxc exec "$BLAZN_QUALIFICATION_TARGET" --user "$account_uid" --group "$account_gid" -- sudo -n /usr/local/bin/blazn node-root-observe >/dev/null 2>&1; then observer=allowed; fi
  case "$account_uid:$account_gid" in absent:*|*:absent|0:*|*:0|*[!0-9:]*) qual_die 'service account UID/GID is missing, root, or invalid' ;; esac
  [ "$unit_user" = blazn-node ] || qual_die 'systemd service does not run as blazn-node'
  [ "$process_uid" = "$account_uid" ] || qual_die 'live service process UID differs from account UID'
  [ "$observer" = allowed ] || qual_die 'daemon account cannot perform exact no-input root observation'
  jq -n --arg os "$os_id" --arg version "$os_version" --arg accountUid "$account_uid" --arg accountGid "$account_gid" --arg unitUser "$unit_user" --arg processUid "$process_uid" --arg observer "$observer" \
    '{schemaVersion:1,os:$os,osVersion:$version,service:{accountUid:$accountUid,accountGid:$accountGid,unitUser:$unitUser,processUid:$processUid},noInputRootObservation:$observer}'
else
  BLAZN_QUALIFICATION_REQUIRE_ACTIVE_SERVICE=1 "$qual_dir/native-mac-preflight.sh"
fi

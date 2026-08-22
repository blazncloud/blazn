#!/usr/bin/env bash
# shellcheck source-path=SCRIPTDIR

set -o errexit
set -o nounset
set -o pipefail
script_dir=$(unset CDPATH; cd -- "$(dirname -- "$0")" && pwd -P)
# shellcheck source=lib/common.sh
source "$script_dir/lib/common.sh"

qual_require_target
[ "${BLAZN_QUALIFICATION_PROFILE:-}" = native-mac ] || qual_die 'native Mac preflight requires profile native-mac'
[ "$(uname -s)" = Darwin ] || qual_die 'native Mac preflight must execute on mac-mini-3 itself'
[ "$(uname -m)" = arm64 ] || qual_die 'native Mac qualification requires arm64'
actual_host=$(scutil --get LocalHostName 2>/dev/null || hostname -s)
expected_host=${BLAZN_QUALIFICATION_EXPECTED_HOSTNAME:-mac-mini-3}
[ "$actual_host" = "$expected_host" ] || qual_die "host ${actual_host} is not approved native target ${expected_host}"
case "$actual_host" in mac-mini-3|mac-mini-3.local) ;; *) qual_die 'native target is not mac-mini-3' ;; esac

qual_require_command limactl
vm=${BLAZN_QUALIFICATION_LIMA_VM:-}
[ -n "$vm" ] || qual_die 'BLAZN_QUALIFICATION_LIMA_VM is required'
worker=${BLAZN_QUALIFICATION_KUBE_NODE:-}
[ -n "$worker" ] || qual_die 'BLAZN_QUALIFICATION_KUBE_NODE is required'
[ "$worker" = mac-mini-3-agent ] || qual_die 'native canary worker must be mac-mini-3-agent'

limactl list --json | jq -e --arg vm "$vm" 'select(.name == $vm and .status == "Running")' >/dev/null || qual_die 'the exact approved Lima VM is not running'

service_account=_blazn-node
service_uid='absent'
service_state='absent'
process_uid='absent'
unit_user='absent'
if id "$service_account" >/dev/null 2>&1; then service_uid=$(id -u "$service_account"); fi
launch_value=$(launchctl print system/com.blazn.node 2>/dev/null || true)
if [ -n "$launch_value" ]; then
  service_state=present
  main_pid=$(awk '$1 == "pid" && $2 == "=" {print $3; exit}' <<<"$launch_value")
  case "$main_pid" in ''|0|*[!0-9]*) ;; *) process_uid=$(ps -o uid= -p "$main_pid" | tr -d ' ') ;; esac
fi
plist=/Library/LaunchDaemons/com.blazn.node.plist
if [ -f "$plist" ]; then unit_user=$(/usr/libexec/PlistBuddy -c 'Print :UserName' "$plist" 2>/dev/null || printf absent); fi
sudo_observe=denied
if [ "$service_uid" != absent ] && sudo -n -u "$service_account" /usr/bin/sudo -n /usr/local/bin/blazn node-root-observe >/dev/null 2>&1; then sudo_observe=allowed; fi

if [ "${BLAZN_QUALIFICATION_REQUIRE_ACTIVE_SERVICE:-0}" = 1 ]; then
  case "$service_uid" in absent|0|*[!0-9]*) qual_die 'native service account UID is missing, root, or invalid' ;; esac
  [ "$service_state" = present ] || qual_die 'native launchd service is absent'
  [ "$unit_user" = "$service_account" ] || qual_die 'launchd plist UserName differs from _blazn-node'
  [ "$process_uid" = "$service_uid" ] || qual_die 'native live process UID differs from service account UID'
  [ "$sudo_observe" = allowed ] || qual_die 'native daemon account cannot perform exact no-input root observation'
fi

jq -n \
  --arg host "$actual_host" --arg arch "$(uname -m)" --arg vm "$vm" --arg worker "$worker" \
  --arg serviceAccount "$service_account" --arg serviceUid "$service_uid" --arg unitUser "$unit_user" --arg processUid "$process_uid" --arg serviceState "$service_state" --arg sudoObserve "$sudo_observe" \
  '{schemaVersion:1,status:"passed",host:$host,architecture:$arch,limaVM:$vm,worker:$worker,service:{account:$serviceAccount,accountUid:$serviceUid,unitUser:$unitUser,processUid:$processUid,state:$serviceState},noInputRootObservation:$sudoObserve}'

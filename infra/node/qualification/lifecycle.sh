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
qual_require_command jq

binary=${BLAZN_QUALIFICATION_BLAZN_BIN:-/usr/local/bin/blazn}
[ "$binary" = /usr/local/bin/blazn ] || qual_die 'qualification requires the frozen /usr/local/bin/blazn service binary'

target_exec() {
  if [ "$BLAZN_QUALIFICATION_PROFILE" = lxd-ubuntu-26.04 ]; then
    qual_guest_name_matches_correlation
    [ "$(lxc config get "$BLAZN_QUALIFICATION_TARGET" user.blazn.qualification 2>/dev/null || true)" = "$BLAZN_QUALIFICATION_CORRELATION_ID" ] || qual_die 'guest correlation marker differs'
    operator_uid=${BLAZN_QUALIFICATION_OPERATOR_UID:-}
    operator_gid=${BLAZN_QUALIFICATION_OPERATOR_GID:-}
    case "$operator_uid:$operator_gid" in *[!0-9:]*|:|*:|0:*) qual_die 'explicit non-root numeric qualification operator UID/GID are required' ;; esac
    lxc exec "$BLAZN_QUALIFICATION_TARGET" --user "$operator_uid" --group "$operator_gid" -- "$@"
  elif [ "$BLAZN_QUALIFICATION_PROFILE" = native-mac ]; then
    [ "$(hostname -s)" = "${BLAZN_QUALIFICATION_EXPECTED_HOSTNAME:-mac-mini-3}" ] || qual_die 'native lifecycle is not running on approved mac-mini-3'
    "$@"
  else
    qual_die 'unsupported qualification profile'
  fi
}

verify_binary() {
  expected=${BLAZN_QUALIFICATION_BINARY_SHA256:-}
  [[ "$expected" =~ ^sha256:[0-9a-f]{64}$ ]] || qual_die 'BLAZN_QUALIFICATION_BINARY_SHA256 must be an exact sha256 digest'
  observed=$(target_exec "$binary" --output=json version | jq -r '.commit // .version // empty')
  [ -n "$observed" ] || qual_die 'released binary did not report version provenance'
  if [ "$BLAZN_QUALIFICATION_PROFILE" = lxd-ubuntu-26.04 ]; then
    payload=$(target_exec sha256sum "$binary" | awk '{print $1}')
  else
    payload=$(shasum -a 256 "$binary" | awk '{print $1}')
  fi
  [ "sha256:${payload}" = "$expected" ] || qual_die 'target binary digest differs from approval'
}

target_daemon_observe() {
  if [ "$BLAZN_QUALIFICATION_PROFILE" = lxd-ubuntu-26.04 ]; then
    daemon_uid=$(lxc exec "$BLAZN_QUALIFICATION_TARGET" -- id -u blazn-node)
    daemon_gid=$(lxc exec "$BLAZN_QUALIFICATION_TARGET" -- id -g blazn-node)
    case "$daemon_uid:$daemon_gid" in 0:*|*:0|*[!0-9:]*) qual_die 'invalid daemon UID/GID for observation' ;; esac
    lxc exec "$BLAZN_QUALIFICATION_TARGET" --user "$daemon_uid" --group "$daemon_gid" -- sudo -n "$binary" node-root-observe
  else
    sudo -n -u _blazn-node /usr/bin/sudo -n "$binary" node-root-observe
  fi
}

verify_plan_expired() {
  expires=${BLAZN_QUALIFICATION_PLAN_EXPIRES_AT:-}
  [ -n "$expires" ] || qual_die 'BLAZN_QUALIFICATION_PLAN_EXPIRES_AT is required for expired-plan gates'
  expiry_epoch=$(python3 - "$expires" <<'PY'
import datetime as dt, sys
try:
    value = dt.datetime.fromisoformat(sys.argv[1].replace("Z", "+00:00"))
except ValueError as exc:
    raise SystemExit(f"invalid plan expiry: {exc}")
if value.tzinfo is None:
    raise SystemExit("plan expiry must include a timezone")
print(int(value.timestamp()))
PY
  )
  if [ "$BLAZN_QUALIFICATION_PROFILE" = lxd-ubuntu-26.04 ]; then
    target_epoch=$(lxc exec "$BLAZN_QUALIFICATION_TARGET" -- date -u +%s)
  else
    target_epoch=$(date -u +%s)
  fi
  [ "$target_epoch" -ge "$expiry_epoch" ] || qual_die 'target clock has not reached the signed plan expiry'
}

validate_install_inputs() {
  for name in BLAZN_QUALIFICATION_WORKSPACE BLAZN_QUALIFICATION_REQUEST_ID BLAZN_QUALIFICATION_MACHINE_FINGERPRINT BLAZN_QUALIFICATION_INSTALL_PROFILE BLAZN_QUALIFICATION_CLUSTER_ID BLAZN_QUALIFICATION_CLUSTER_ORIGIN; do
    [ -n "${!name:-}" ] || qual_die "${name} is required"
  done
  case "$BLAZN_QUALIFICATION_REQUEST_ID" in *[!a-zA-Z0-9_.:-]*|'') qual_die 'request ID has unsafe characters' ;; esac
  case "$BLAZN_QUALIFICATION_WORKSPACE" in *[!a-zA-Z0-9_.:-]*|'') qual_die 'workspace has unsafe characters' ;; esac
  [[ "$BLAZN_QUALIFICATION_MACHINE_FINGERPRINT" =~ ^sha256:[0-9a-f]{64}$ ]] || qual_die 'machine fingerprint must be an exact sha256 digest'
  case "$BLAZN_QUALIFICATION_CLUSTER_ID" in "$qual_shared_cluster_id"|frontro-*|*shared*) qual_die 'shared/Frontro cluster IDs are prohibited' ;; esac
  [ "$BLAZN_QUALIFICATION_CLUSTER_ORIGIN" != "$qual_shared_cluster_origin" ] || qual_die 'known shared Frontro API server is prohibited'
  if [ "$BLAZN_QUALIFICATION_PROFILE" = lxd-ubuntu-26.04 ]; then
    [[ "$BLAZN_QUALIFICATION_INSTALL_PROFILE" =~ ^/etc/blazn/node/profiles/[a-zA-Z0-9_.-]+\.json$ ]] || qual_die 'Linux install profile must be one JSON file under the frozen profile root'
  else
    [[ "$BLAZN_QUALIFICATION_INSTALL_PROFILE" =~ ^/Library/Application\ Support/BlaznNodeRoot/profiles/[a-zA-Z0-9_.-]+\.json$ ]] || qual_die 'Mac install profile must be one JSON file under the frozen profile root'
    for name in BLAZN_QUALIFICATION_KUBE_NODE BLAZN_QUALIFICATION_EXPECTED_NODE_UID BLAZN_QUALIFICATION_EXPECTED_RESOURCE_VERSION; do
      [ -n "${!name:-}" ] || qual_die "${name} is required for native adoption"
    done
  fi
  # shellcheck disable=SC2016
  target_exec jq -e --arg origin "$BLAZN_QUALIFICATION_CLUSTER_ORIGIN" \
    '.allowedClusterOrigins == [$origin]' "$BLAZN_QUALIFICATION_INSTALL_PROFILE" >/dev/null || qual_die 'trusted profile does not pin exactly the approved disposable cluster origin'
  if [ "$BLAZN_QUALIFICATION_PROFILE" = native-mac ]; then
    # shellcheck disable=SC2016
    target_exec jq -e --arg cluster "$BLAZN_QUALIFICATION_CLUSTER_ID" \
      '.limaBinding.clusterId == $cluster' "$BLAZN_QUALIFICATION_INSTALL_PROFILE" >/dev/null || qual_die 'native trusted profile Lima binding differs from approved disposable cluster ID'
  fi
}

build_install_args() {
  request_id=$1
  mode=fresh
  enrollment_name=$BLAZN_QUALIFICATION_TARGET
  if [ "$BLAZN_QUALIFICATION_PROFILE" = native-mac ]; then
    mode=adopt
    enrollment_name=$BLAZN_QUALIFICATION_KUBE_NODE
  fi
  install_args=("$binary" --output=json node enroll
    --workspace "$BLAZN_QUALIFICATION_WORKSPACE"
    --request-id "$request_id"
    --name "$enrollment_name"
    --mode "$mode"
    --machine-fingerprint "$BLAZN_QUALIFICATION_MACHINE_FINGERPRINT"
    --profile "$BLAZN_QUALIFICATION_INSTALL_PROFILE")
  if [ "$mode" = adopt ]; then
    install_args+=(--cluster-id "$BLAZN_QUALIFICATION_CLUSTER_ID"
      --node-name "$BLAZN_QUALIFICATION_KUBE_NODE"
      --node-uid "$BLAZN_QUALIFICATION_EXPECTED_NODE_UID"
      --resource-version "$BLAZN_QUALIFICATION_EXPECTED_RESOURCE_VERSION")
  fi
}

run_install() {
  build_install_args "$1"
  target_exec "${install_args[@]}"
}

run_crash_case() {
  crash_lifecycle=$1
  checkpoint=$2
  [ "$BLAZN_QUALIFICATION_PROFILE" = lxd-ubuntu-26.04 ] || qual_die 'crash cases are restricted to the disposable LXD guest'
  snapshot=${BLAZN_QUALIFICATION_SNAPSHOT:-}
  [[ "$snapshot" =~ ^checkpoint-[a-z0-9][a-z0-9-]{1,47}$ ]] || qual_die 'crash case requires the exact reviewed LXD snapshot name'
  lxc info "${BLAZN_QUALIFICATION_TARGET}/${snapshot}" >/dev/null 2>&1 || qual_die 'reviewed recovery snapshot does not exist'
  verify_binary
  if [ "$crash_lifecycle" = install ]; then
    validate_install_inputs
    build_install_args "$BLAZN_QUALIFICATION_REQUEST_ID"
    crash_command=("${install_args[@]}")
  else
    crash_command=("$binary" --output=json node uninstall --yes --remove-managed-runtime)
  fi
  suffix=${BLAZN_QUALIFICATION_CORRELATION_ID#nodequal-}
  pidfile="/tmp/.blazn-qualification-${suffix}.pid"
  if lxc exec "$BLAZN_QUALIFICATION_TARGET" -- test -e "$pidfile"; then qual_die 'stale qualification PID file exists'; fi
  operator_uid=${BLAZN_QUALIFICATION_OPERATOR_UID:-}
  operator_gid=${BLAZN_QUALIFICATION_OPERATOR_GID:-}
  case "$operator_uid:$operator_gid" in *[!0-9:]*|:|*:|0:*) qual_die 'explicit non-root numeric qualification operator UID/GID are required' ;; esac
  crash_tmp=$(mktemp -d)
  trap 'rm -rf -- "$crash_tmp"' EXIT
  # shellcheck disable=SC2016
  lxc exec "$BLAZN_QUALIFICATION_TARGET" --user "$operator_uid" --group "$operator_gid" -- \
    bash -c 'set -o errexit; set -o noclobber; umask 077; printf "%s\n" "$$" >"$1"; shift; exec "$@"' \
    qualification-crash "$pidfile" "${crash_command[@]}" >"$crash_tmp/stdout" 2>"$crash_tmp/stderr" &
  lxc_client_pid=$!
  timeout=${BLAZN_QUALIFICATION_CRASH_TIMEOUT_SECONDS:-300}
  case "$timeout" in ''|*[!0-9]*) qual_die 'crash timeout must be numeric' ;; esac
  [ "$timeout" -ge 30 ] && [ "$timeout" -le 900 ] || qual_die 'crash timeout must be 30-900 seconds'
  deadline=$((SECONDS + timeout))
  target_pid=''
  observed=''
  monitor_error=''
  while kill -0 "$lxc_client_pid" 2>/dev/null && [ "$SECONDS" -lt "$deadline" ]; do
    target_pid=$(lxc exec "$BLAZN_QUALIFICATION_TARGET" -- cat "$pidfile" 2>/dev/null || true)
    if [[ "$target_pid" =~ ^[0-9]+$ ]] && [ "$target_pid" -gt 1 ]; then
      observed=$(lxc exec "$BLAZN_QUALIFICATION_TARGET" -- jq -r '.lifecycle + ":" + .checkpoint' /var/lib/blazn-node-root/install-wal.json 2>/dev/null || true)
      if [ "$crash_lifecycle" = cleanup ] && [ "${observed%%:*}" = uninstall ]; then observed="cleanup:${observed#*:}"; fi
      if [ "$observed" = "${crash_lifecycle}:${checkpoint}" ]; then
        comm=$(lxc exec "$BLAZN_QUALIFICATION_TARGET" -- cat "/proc/${target_pid}/comm" 2>/dev/null || true)
        case "$comm" in blazn|blazn.real) ;; *) monitor_error="target PID ${target_pid} is not an exact Blazn process"; break ;; esac
        lxc exec "$BLAZN_QUALIFICATION_TARGET" -- kill -KILL "$target_pid"
        break
      fi
    fi
    sleep 0.05
  done
  if [ -n "$monitor_error" ]; then
    wait "$lxc_client_pid" || true
    lxc exec "$BLAZN_QUALIFICATION_TARGET" -- rm -f -- "$pidfile"
    qual_die "$monitor_error"
  fi
  if [ "$observed" != "${crash_lifecycle}:${checkpoint}" ]; then
    wait "$lxc_client_pid" || true
    lxc exec "$BLAZN_QUALIFICATION_TARGET" -- rm -f -- "$pidfile"
    qual_die "approved crash checkpoint was not observed (last=${observed:-absent})"
  fi
  if wait "$lxc_client_pid"; then qual_die 'crashed lifecycle command unexpectedly exited successfully'; fi
  lxc exec "$BLAZN_QUALIFICATION_TARGET" -- rm -- "$pidfile"
  if [ "$crash_lifecycle" = install ]; then
    recovery=$(target_exec "$binary" --output=json node recover)
  else
    recovery=$(target_exec "$binary" --output=json node uninstall --yes --remove-managed-runtime)
  fi
  jq -n --arg lifecycle "$crash_lifecycle" --arg checkpoint "$checkpoint" --argjson pid "$target_pid" --argjson recovery "$recovery" \
    '{schemaVersion:1,status:"passed",crash:{lifecycle:$lifecycle,checkpoint:$checkpoint,pid:$pid},recovery:$recovery}'
}

do_action() {
  verify_binary
  case "$action" in
    install|idempotent-install)
      validate_install_inputs
      run_install "$BLAZN_QUALIFICATION_REQUEST_ID"
      ;;
    reinstall)
      validate_install_inputs
      reinstall_id=${BLAZN_QUALIFICATION_REINSTALL_REQUEST_ID:-}
      [ -n "$reinstall_id" ] && [ "$reinstall_id" != "$BLAZN_QUALIFICATION_REQUEST_ID" ] || qual_die 'reinstall requires a distinct BLAZN_QUALIFICATION_REINSTALL_REQUEST_ID'
      case "$reinstall_id" in *[!a-zA-Z0-9_.:-]*) qual_die 'reinstall request ID has unsafe characters' ;; esac
      run_install "$reinstall_id"
      ;;
    repair)
      target_exec "$binary" --output=json node repair
      ;;
    expired-observe)
      verify_plan_expired
      target_daemon_observe
      ;;
    expired-repair-denied)
      verify_plan_expired
      repair_output=''
      if repair_output=$(target_exec "$binary" --output=json node repair); then
        qual_die 'repair unexpectedly accepted an expired plan'
      fi
      qual_require_expired_repair_denial "$repair_output"
      jq -n --argjson denial "$repair_output" '{schemaVersion:1,status:"passed",expiredRepairDenied:true,denial:$denial}'
      ;;
    expired-uninstall|uninstall)
      if [ "$action" = expired-uninstall ]; then verify_plan_expired; fi
      uninstall_args=("$binary" --output=json node uninstall --yes)
      if [ "$BLAZN_QUALIFICATION_PROFILE" = lxd-ubuntu-26.04 ]; then uninstall_args+=(--remove-managed-runtime); fi
      target_exec "${uninstall_args[@]}"
      ;;
    *) qual_die "unsupported lifecycle action: ${action}" ;;
  esac
}

case "$action" in
  plan)
    printf '%s\n' \
      'install -> idempotent-install -> repair' \
      'wait for signed plan expiry -> expired-observe -> expired-repair-denied -> expired-uninstall' \
      'restore disposable snapshot -> install crash/recover cases -> cleanup crash/recover cases -> reinstall' \
      'each mutation requires a new correlation-bound action approval'
    ;;
  install|idempotent-install|reinstall|repair|expired-repair-denied|expired-uninstall|uninstall)
    qual_require_approval "lifecycle-${action}"
    qual_with_lock do_action
    ;;
  crash-install-*|crash-cleanup-*)
    crash_lifecycle=${action#crash-}
    crash_lifecycle=${crash_lifecycle%%-*}
    checkpoint=${action#crash-"${crash_lifecycle}"-}
    case "${crash_lifecycle}:${checkpoint}" in
      install:join_intent|install:join|install:binding|install:broker_consume|install:broker_consumed|install:verify|install:receipt) ;;
      cleanup:cleanup_pending|cleanup:cleanup_support_removed|cleanup:cleanup_local_state_removed) ;;
      *) qual_die 'crash action is not in the reviewed checkpoint allowlist' ;;
    esac
    qual_require_approval "$action"
    qual_with_lock run_crash_case "$crash_lifecycle" "$checkpoint"
    ;;
  expired-observe)
    do_action
    ;;
  *) qual_die 'usage: lifecycle.sh plan|install|idempotent-install|repair|expired-observe|expired-repair-denied|expired-uninstall|uninstall|reinstall|crash-install-CHECKPOINT|crash-cleanup-CHECKPOINT' ;;
esac

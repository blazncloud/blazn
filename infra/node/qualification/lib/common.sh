#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

qual_dir=$(unset CDPATH; cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
repo_root=$(unset CDPATH; cd -- "${qual_dir}/../../.." && pwd -P)
readonly qual_shared_cluster_id='frontro-microk8s-8f109e68-f1bf-40e5-8482-c97d10997dc2'
readonly qual_shared_cluster_origin='https://192.168.0.108:16443'

qual_die() {
  printf 'node qualification: %s\n' "$*" >&2
  exit 1
}

qual_note() {
  printf 'node qualification: %s\n' "$*" >&2
}

qual_require_command() {
  command -v "$1" >/dev/null 2>&1 || qual_die "required command is unavailable: $1"
}

qual_require_correlation() {
  [[ "${BLAZN_QUALIFICATION_CORRELATION_ID:-}" =~ ^nodequal-[a-z0-9][a-z0-9.-]{6,63}$ ]] || qual_die 'BLAZN_QUALIFICATION_CORRELATION_ID must match nodequal-[a-z0-9][a-z0-9.-]{6,63}'
  [ "${#BLAZN_QUALIFICATION_CORRELATION_ID}" -le 72 ] || qual_die 'correlation ID is longer than 72 characters'
}

qual_require_target() {
  qual_require_correlation
  case "${BLAZN_QUALIFICATION_TARGET:-}" in
    ben4|ben4.*|frontro-agent-worker|frontro-agent-worker.*)
      qual_die 'the ben4 host and shared frontro-agent-worker VM are never qualification targets'
      ;;
    blazn-q-*) ;;
    mac-mini-3|mac-mini-3.*)
      [ "${BLAZN_QUALIFICATION_PROFILE:-}" = native-mac ] || qual_die 'mac-mini-3 requires BLAZN_QUALIFICATION_PROFILE=native-mac'
      ;;
    *) qual_die 'target must be a correlation-bound blazn-q-* guest or mac-mini-3' ;;
  esac
}

qual_is_mutation() {
  [ "${BLAZN_QUALIFICATION_MODE:-dry-run}" = mutate ]
}

qual_require_approval() {
  action=$1
  qual_require_target
  qual_is_mutation || qual_die "${action} requires BLAZN_QUALIFICATION_MODE=mutate"
  qual_export_lock_identity
  input_digest=$(qual_approval_input_digest "$action")
  expected="APPROVE:${BLAZN_QUALIFICATION_CORRELATION_ID}:${BLAZN_QUALIFICATION_TARGET}:${action}:${input_digest}"
  [ "${BLAZN_QUALIFICATION_APPROVAL:-}" = "$expected" ] || qual_die "approval must equal ${expected}"
  [ "${BLAZN_QUALIFICATION_APPROVED_HEAD:-}" = "$(git -C "$repo_root" rev-parse HEAD)" ] || qual_die 'approval is not bound to the current source HEAD'
  [ "$(git -C "$repo_root" remote get-url origin)" = 'https://github.com/blazncloud/blazn.git' ] || qual_die 'origin is not the canonical blazncloud repository'
  [ -z "$(git -C "$repo_root" status --porcelain --untracked-files=all)" ] || qual_die 'source is dirty; mutation evidence would not be reproducible'
  BLAZN_QUALIFICATION_ACCEPTED_INPUT_DIGEST=$input_digest
  export BLAZN_QUALIFICATION_ACCEPTED_INPUT_DIGEST
}

qual_approval_input_digest() {
  approval_action=$1
  qual_require_command python3
  approval_head=$(git -C "$repo_root" rev-parse HEAD)
  python3 - "$approval_action" "$approval_head" <<'PY'
import hashlib, json, os, sys

# These are the complete non-secret inputs which can change the target or the
# meaning/scope of a qualification mutation. Missing values are bound as empty
# strings so setting one after approval always invalidates the approval.
names = (
    "BLAZN_QUALIFICATION_BINARY_SHA256",
    "BLAZN_QUALIFICATION_CLUSTER_ID",
    "BLAZN_QUALIFICATION_CLUSTER_ORIGIN",
    "BLAZN_QUALIFICATION_EXPECTED_NODE_UID",
    "BLAZN_QUALIFICATION_EXPECTED_RESOURCE_VERSION",
    "BLAZN_QUALIFICATION_EXPECTED_HOSTNAME",
    "BLAZN_QUALIFICATION_INSTALL_PROFILE",
    "BLAZN_QUALIFICATION_INSTALL_PROFILE_SHA256",
    "BLAZN_QUALIFICATION_KUBE_CONTEXT",
    "BLAZN_QUALIFICATION_KUBE_NODE",
    "BLAZN_QUALIFICATION_KUBE_SYSTEM_UID",
    "BLAZN_QUALIFICATION_LIMA_VM",
    "BLAZN_QUALIFICATION_LXD_CPU",
    "BLAZN_QUALIFICATION_LXD_IMAGE_FINGERPRINT",
    "BLAZN_QUALIFICATION_LXD_MEMORY",
    "BLAZN_QUALIFICATION_LXD_PROCESSES",
    "BLAZN_QUALIFICATION_LXD_ROOT_DISK",
    "BLAZN_QUALIFICATION_MACHINE_FINGERPRINT",
    "BLAZN_QUALIFICATION_OPERATOR_GID",
    "BLAZN_QUALIFICATION_OPERATOR_UID",
    "BLAZN_QUALIFICATION_PLAN_EXPIRES_AT",
    "BLAZN_QUALIFICATION_PROFILE",
    "BLAZN_QUALIFICATION_REINSTALL_REQUEST_ID",
    "BLAZN_QUALIFICATION_REQUEST_ID",
    "BLAZN_QUALIFICATION_SNAPSHOT",
    "BLAZN_QUALIFICATION_SNAPSHOT_IDENTITY_SHA256",
    "BLAZN_QUALIFICATION_CLEAN_TARGET_STATE_SHA256",
    "BLAZN_QUALIFICATION_TARGET",
    "BLAZN_QUALIFICATION_WORKSPACE",
)
document = {
    "action": sys.argv[1],
    "sourceHead": sys.argv[2],
    "inputs": {name: os.environ.get(name, "") for name in names},
    "lock": {
        "path": os.environ.get("BLAZN_QUALIFICATION_LOCK_FILE", ""),
        "identity": os.environ.get("BLAZN_QUALIFICATION_LOCK_IDENTITY", ""),
    },
    "crashTimeoutSeconds": os.environ.get("BLAZN_QUALIFICATION_CRASH_TIMEOUT_SECONDS", ""),
}
payload = json.dumps(document, sort_keys=True, separators=(",", ":")).encode()
print("sha256:" + hashlib.sha256(payload).hexdigest())
PY
}

qual_validate_lxd_limits() {
  lxd_cpu=${BLAZN_QUALIFICATION_LXD_CPU:-4}
  lxd_memory=${BLAZN_QUALIFICATION_LXD_MEMORY:-8GiB}
  lxd_root_disk=${BLAZN_QUALIFICATION_LXD_ROOT_DISK:-32GiB}
  lxd_processes=${BLAZN_QUALIFICATION_LXD_PROCESSES:-1024}
  case "$lxd_cpu" in ''|*[!0-9]*) qual_die 'LXD CPU limit must be an integer from 1 through 8' ;; esac
  [ "$lxd_cpu" -ge 1 ] && [ "$lxd_cpu" -le 8 ] || qual_die 'LXD CPU limit must be an integer from 1 through 8'
  [[ "$lxd_memory" =~ ^([1-9]|1[0-6])GiB$ ]] || qual_die 'LXD memory limit must be an integer GiB value from 1GiB through 16GiB'
  [[ "$lxd_root_disk" =~ ^(1[6-9]|[2-5][0-9]|6[0-4])GiB$ ]] || qual_die 'LXD root disk limit must be an integer GiB value from 16GiB through 64GiB'
  case "$lxd_processes" in ''|*[!0-9]*) qual_die 'LXD process limit must be an integer from 256 through 2048' ;; esac
  [ "$lxd_processes" -ge 256 ] && [ "$lxd_processes" -le 2048 ] || qual_die 'LXD process limit must be an integer from 256 through 2048'
  BLAZN_QUALIFICATION_LXD_CPU=$lxd_cpu
  BLAZN_QUALIFICATION_LXD_MEMORY=$lxd_memory
  BLAZN_QUALIFICATION_LXD_ROOT_DISK=$lxd_root_disk
  BLAZN_QUALIFICATION_LXD_PROCESSES=$lxd_processes
  export BLAZN_QUALIFICATION_LXD_CPU BLAZN_QUALIFICATION_LXD_MEMORY BLAZN_QUALIFICATION_LXD_ROOT_DISK BLAZN_QUALIFICATION_LXD_PROCESSES
}

qual_require_expired_repair_denial() {
  denial=$1
  jq -e '.exitCode == 1 and .error.code == "node_failed" and .error.message == "repair requires an authorized fresh, unexpired plan: install plan is not active at trusted current time"' <<<"$denial" >/dev/null ||
    qual_die 'repair failed, but not with the exact expired-plan denial envelope'
}

qual_require_stale_cas_rejection() {
  rejection=$1
  if jq -e '(.kind == "Status") and (.status == "Failure") and (.reason == "Invalid") and (.code == 422) and (.message | test("(?i)jsonpatch test operation does not apply.*resourceVersion|resourceVersion.*jsonpatch test operation does not apply"))' <<<"$rejection" >/dev/null 2>&1; then
    jq -n --argjson status "$rejection" '{classification:"kubernetes-status-invalid-422-jsonpatch-test",reason:"Invalid",code:422,status:$status}'
    return
  fi
  if [[ "$rejection" =~ ^Error\ from\ server\ \(Invalid\): ]] &&
      [[ "$rejection" =~ [Jj][Ss][Oo][Nn][Pp]atch\ test\ operation\ does\ not\ apply ]] &&
      [[ "$rejection" =~ resourceVersion ]]; then
    jq -n --arg message "$rejection" '{classification:"kubectl-invalid-jsonpatch-test",reason:"Invalid",message:$message}'
    return
  fi
  qual_die 'stale CAS failed, but not with the exact JSON Patch test rejection'
}

qual_export_lock_identity() {
  lock_file=${BLAZN_QUALIFICATION_LOCK_FILE:-}
  [ -n "$lock_file" ] || qual_die 'BLAZN_QUALIFICATION_LOCK_FILE is required for mutation approval'
  [ -f "$lock_file" ] && [ ! -L "$lock_file" ] || qual_die 'the approval-bound qualification lock must be a regular non-symlink file'
  if stat -c '%d:%i:%u:%a' "$lock_file" >/dev/null 2>&1; then
    BLAZN_QUALIFICATION_LOCK_IDENTITY=$(stat -c '%d:%i:%u:%a' "$lock_file")
  else
    BLAZN_QUALIFICATION_LOCK_IDENTITY=$(stat -f '%d:%i:%u:%Lp' "$lock_file")
  fi
  export BLAZN_QUALIFICATION_LOCK_IDENTITY
}

qual_snapshot_identity_digest() {
  instance_uuid=$1
  snapshot_name=$2
  snapshot_created_at=$3
  config_digest=$4
  clean_state_digest=$5
  [[ "$instance_uuid" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]] || qual_die 'LXD instance UUID is unavailable or invalid'
  [[ "$snapshot_name" =~ ^checkpoint-[a-z0-9][a-z0-9-]{1,47}$ ]] || qual_die 'snapshot identity name is invalid'
  [[ "$config_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || qual_die 'snapshot config digest is invalid'
  [[ "$clean_state_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || qual_die 'clean target-state digest is invalid'
  python3 - "$instance_uuid" "$snapshot_name" "$snapshot_created_at" "$config_digest" "$clean_state_digest" <<'PY'
import hashlib, json, sys
value = {"instanceUuid": sys.argv[1], "snapshot": sys.argv[2], "snapshotCreatedAt": sys.argv[3], "configDigest": sys.argv[4], "cleanTargetStateDigest": sys.argv[5]}
print("sha256:" + hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest())
PY
}

qual_lxd_snapshot_identity() {
  identity_guest=$1
  identity_snapshot=$2
  clean_digest=${BLAZN_QUALIFICATION_CLEAN_TARGET_STATE_SHA256:-}
  [[ "$clean_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || qual_die 'approval-bound clean target-state digest is required'
  instance_uuid=$(lxc config get "$identity_guest" volatile.uuid)
  snapshot_info=$(lxc info "${identity_guest}/${identity_snapshot}" --format=json)
  snapshot_created=$(jq -r '.created_at // .createdAt // empty' <<<"$snapshot_info")
  [ -n "$snapshot_created" ] || qual_die 'snapshot creation identity is unavailable'
  snapshot_config=$(lxc config show "${identity_guest}/${identity_snapshot}" --expanded)
  config_digest="sha256:$(printf '%s' "$snapshot_config" | sha256sum | awk '{print $1}')"
  identity_digest=$(qual_snapshot_identity_digest "$instance_uuid" "$identity_snapshot" "$snapshot_created" "$config_digest" "$clean_digest")
  jq -n --arg instanceUuid "$instance_uuid" --arg snapshot "$identity_snapshot" --arg snapshotCreatedAt "$snapshot_created" --arg configDigest "$config_digest" --arg cleanTargetStateDigest "$clean_digest" --arg identityDigest "$identity_digest" \
    '{instanceUuid:$instanceUuid,snapshot:$snapshot,snapshotCreatedAt:$snapshotCreatedAt,configDigest:$configDigest,cleanTargetStateDigest:$cleanTargetStateDigest,identityDigest:$identityDigest}'
}

qual_validate_lock() {
  lock_file=${BLAZN_QUALIFICATION_LOCK_FILE:-}
  [ -n "$lock_file" ] || qual_die 'BLAZN_QUALIFICATION_LOCK_FILE is required for mutation'
  case "$lock_file" in
    /var/lock/blazn-qualification/*|/run/lock/blazn-qualification/*) ;;
    *) qual_die 'mutation lock must be beneath /var/lock/blazn-qualification or /run/lock/blazn-qualification' ;;
  esac
  [ -f "$lock_file" ] && [ ! -L "$lock_file" ] || qual_die 'the pre-created qualification lock must be a regular non-symlink file'
  if stat -c '%u %a' "$lock_file" >/dev/null 2>&1; then
    lock_stat=$(stat -c '%u %a' "$lock_file")
  else
    lock_stat=$(stat -f '%u %Lp' "$lock_file")
  fi
  case "$lock_stat" in
    '0 600'|'0 640'|'0 644') ;;
    *) qual_die "qualification lock must be root-owned and not writable by group/other (observed ${lock_stat})" ;;
  esac
}

qual_with_lock() {
  qual_validate_lock
  qual_require_command flock
  exec {qual_lock_fd}<>"$BLAZN_QUALIFICATION_LOCK_FILE"
  flock -n "$qual_lock_fd" || qual_die 'qualification lifecycle lock is held by another operator'
  "$@"
}

qual_guest_name_matches_correlation() {
  suffix=${BLAZN_QUALIFICATION_CORRELATION_ID#nodequal-}
  [[ "$suffix" =~ ^[a-z0-9][a-z0-9-]{6,55}$ ]] || qual_die 'LXD correlation suffix must be a DNS-safe lowercase label'
  case "$BLAZN_QUALIFICATION_TARGET" in
    blazn-q-"$suffix"|blazn-q-"$suffix"-*) ;;
    *) qual_die 'disposable guest name is not bound to the correlation ID' ;;
  esac
}

qual_refuse_shared_cluster() {
  qual_require_command kubectl
  expected_context=${BLAZN_QUALIFICATION_KUBE_CONTEXT:-}
  [ -n "$expected_context" ] || qual_die 'BLAZN_QUALIFICATION_KUBE_CONTEXT is required'
  current_context=$(kubectl config current-context)
  [ "$current_context" = "$expected_context" ] || qual_die "kubectl context ${current_context} differs from approved ${expected_context}"
  lower_context=$(printf '%s' "$current_context" | tr '[:upper:]' '[:lower:]')
  case "$lower_context" in
    *frontro*|*microk8s*|*ben1*|*shared*) qual_die 'shared/frontro Kubernetes contexts are prohibited' ;;
  esac
  expected_cluster_id=${BLAZN_QUALIFICATION_CLUSTER_ID:-}
  expected_origin=${BLAZN_QUALIFICATION_CLUSTER_ORIGIN:-}
  expected_system_uid=${BLAZN_QUALIFICATION_KUBE_SYSTEM_UID:-}
  [ -n "$expected_cluster_id" ] && [ -n "$expected_origin" ] && [ -n "$expected_system_uid" ] || qual_die 'exact disposable cluster ID, API origin, and kube-system UID are required'
  case "$expected_cluster_id" in "$qual_shared_cluster_id"|frontro-*|*shared*) qual_die 'shared/Frontro cluster IDs are prohibited' ;; esac
  [ "$expected_origin" != "$qual_shared_cluster_origin" ] || qual_die 'the known shared Frontro API server is prohibited'
  observed_origin=$(kubectl --context "$expected_context" config view --minify -o 'jsonpath={.clusters[0].cluster.server}')
  [ "$observed_origin" = "$expected_origin" ] || qual_die 'Kubernetes API origin differs from the approved disposable origin'
  observed_system_uid=$(kubectl --context "$expected_context" get namespace kube-system -o 'jsonpath={.metadata.uid}')
  [ "$observed_system_uid" = "$expected_system_uid" ] || qual_die 'kube-system UID differs from the approved disposable cluster identity'
  marker=$(kubectl --context "$expected_context" get namespace blazn-qualification -o 'jsonpath={.metadata.annotations.blazn\.dev/qualification-correlation}' 2>/dev/null || true)
  [ "$marker" = "$BLAZN_QUALIFICATION_CORRELATION_ID" ] || qual_die 'cluster lacks the exact disposable qualification correlation marker'
}

qual_sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

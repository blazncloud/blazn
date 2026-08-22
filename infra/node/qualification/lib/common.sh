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
  expected="APPROVE:${BLAZN_QUALIFICATION_CORRELATION_ID}:${BLAZN_QUALIFICATION_TARGET}:${action}"
  [ "${BLAZN_QUALIFICATION_APPROVAL:-}" = "$expected" ] || qual_die "approval must equal ${expected}"
  [ "${BLAZN_QUALIFICATION_APPROVED_HEAD:-}" = "$(git -C "$repo_root" rev-parse HEAD)" ] || qual_die 'approval is not bound to the current source HEAD'
  [ "$(git -C "$repo_root" remote get-url origin)" = 'https://github.com/blazncloud/blazn.git' ] || qual_die 'origin is not the canonical blazncloud repository'
  [ -z "$(git -C "$repo_root" status --porcelain --untracked-files=all)" ] || qual_die 'source is dirty; mutation evidence would not be reproducible'
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

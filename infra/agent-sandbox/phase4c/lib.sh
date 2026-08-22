#!/bin/sh

phase4c_require_identity() {
  : "${BLAZN_EXPECTED_CONTEXT:?set exact reviewed kube context}"
  : "${BLAZN_EXPECTED_KUBE_SYSTEM_UID:?set kube-system UID from inventory}"
  actual_context=$(kubectl config current-context)
  actual_uid=$(kubectl get namespace kube-system -o jsonpath='{.metadata.uid}')
  [ "$actual_context" = "$BLAZN_EXPECTED_CONTEXT" ] || {
    printf 'kube context changed: expected %s, got %s\n' "$BLAZN_EXPECTED_CONTEXT" "$actual_context" >&2
    return 1
  }
  [ "$actual_uid" = "$BLAZN_EXPECTED_KUBE_SYSTEM_UID" ] || {
    printf 'cluster UID changed\n' >&2
    return 1
  }
}

phase4c_require_mutation_authority() {
  phase4c_require_identity
  [ "${BLAZN_PHASE4C_CHANGE_APPROVED:-}" = 'approved-phase4c-live-canary' ] || {
    printf 'explicit Phase 4C change approval is required\n' >&2
    return 1
  }
  case "${BLAZN_FENCING_TOKEN:-}" in ''|*[!0-9]*) printf 'numeric fencing token is required\n' >&2; return 1 ;; esac
  [ "${BLAZN_LIVE_CLUSTER_LOCK_HELD:-}" = "token:$BLAZN_FENCING_TOKEN" ] || {
    printf 'serialized live-cluster lock proof is required\n' >&2
    return 1
  }
  [ -r "/proc/$$/fd/9" ] || {
    printf 'live-cluster lock must be inherited on file descriptor 9\n' >&2
    return 1
  }
  lock_path=/run/lock/blazn/live-cluster-mutation.lock
  [ "$(readlink "/proc/$$/fd/9")" = "$lock_path" ] || {
    printf 'file descriptor 9 is not the authoritative live-cluster lock\n' >&2
    return 1
  }
  [ "$(stat -c '%u:%a' "$lock_path")" = '0:600' ] || {
    printf 'live-cluster lock ownership or mode is unsafe\n' >&2
    return 1
  }
  [ "$(cat /run/lock/blazn/live-cluster-mutation.fence)" = "$BLAZN_FENCING_TOKEN" ] || {
    printf 'fencing token is stale\n' >&2
    return 1
  }
}

phase4c_count() {
  kubectl get "$@" --no-headers 2>/dev/null | wc -l | tr -d ' '
}

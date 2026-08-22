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
  lock_id=$(stat -Lc '%d:%i' "/proc/$$/fd/9")
  if [ -z "${BLAZN_LIVE_CLUSTER_LOCK_ID:-}" ] || [ "$lock_id" != "$BLAZN_LIVE_CLUSTER_LOCK_ID" ]; then
    printf 'inherited live-cluster lock inode identity changed\n' >&2
    return 1
  fi
  [ "$(stat -Lc '%u:%a:%h' "/proc/$$/fd/9")" = '0:600:1' ] || {
    printf 'inherited live-cluster lock metadata is unsafe\n' >&2
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

phase4c_write_phase() {
  transaction=$1
  next_phase=$2
  tmp_phase=$(mktemp "$transaction/.phase.XXXXXX")
  printf '%s\n' "$next_phase" >"$tmp_phase"
  chmod 0600 "$tmp_phase"
  sync -f "$tmp_phase"
  mv "$tmp_phase" "$transaction/phase"
  sync -f "$transaction"
  if [ -n "${BLAZN_PHASE4C_FAIL_AFTER:-}" ] && [ "$BLAZN_PHASE4C_FAIL_AFTER" = "$next_phase" ]; then
    [ "${BLAZN_PHASE4C_DISPOSABLE_TEST:-}" = 'true' ] || { printf 'failpoints require disposable test mode\n' >&2; return 1; }
    printf 'disposable failpoint after %s\n' "$next_phase" >&2
    return 86
  fi
}

phase4c_verify_transaction() {
  transaction=$1
  if [ ! -d "$transaction" ] || [ -L "$transaction" ] || [ "$(stat -c '%u:%a' "$transaction")" != '0:700' ]; then
    printf 'transaction directory metadata is unsafe\n' >&2
    return 1
  fi
  : "${BLAZN_REVIEWED_INPUT_DIGEST:?set the separately reviewed transaction input digest}"
  [ "$(cat "$transaction/input.digest")" = "$BLAZN_REVIEWED_INPUT_DIGEST" ] || { printf 'reviewed input digest mismatch\n' >&2; return 1; }
  (cd "$transaction" && sha256sum -c input.sha256 >/dev/null) || { printf 'sealed transaction input changed\n' >&2; return 1; }
}

phase4c_start_uid_proxy() {
  transaction=$1
  command -v curl >/dev/null 2>&1 || { printf 'curl is required for UID-precondition deletes\n' >&2; return 1; }
  phase4c_proxy_dir=$(mktemp -d "$transaction/.api-proxy.XXXXXX")
  chmod 0700 "$phase4c_proxy_dir"
  phase4c_proxy_socket=$phase4c_proxy_dir/kubernetes-api.sock
  kubectl proxy --unix-socket="$phase4c_proxy_socket" --api-prefix=/ --accept-hosts='^localhost$' >"$phase4c_proxy_dir/kubectl-proxy.log" 2>&1 &
  phase4c_proxy_pid=$!
  attempt=0
  while [ ! -S "$phase4c_proxy_socket" ]; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 50 ] || ! kill -0 "$phase4c_proxy_pid" 2>/dev/null; then printf 'private Kubernetes API proxy did not start\n' >&2; return 1; fi
    sleep 0.1
  done
  chmod 0600 "$phase4c_proxy_socket"
}

phase4c_stop_uid_proxy() {
  if [ -n "${phase4c_proxy_pid:-}" ]; then
    kill "$phase4c_proxy_pid" 2>/dev/null || :
    wait "$phase4c_proxy_pid" 2>/dev/null || :
    phase4c_proxy_pid=''
  fi
}

phase4c_delete_uid() {
  api_path=$1
  expected_uid=$2
  case "$expected_uid" in ????????-????-????-????-????????????) ;; *) printf 'invalid deletion UID\n' >&2; return 1 ;; esac
  payload=$(printf '{"apiVersion":"v1","kind":"DeleteOptions","propagationPolicy":"Foreground","preconditions":{"uid":"%s"}}' "$expected_uid")
  curl --fail-with-body --silent --show-error --unix-socket "$phase4c_proxy_socket" \
    -X DELETE -H 'content-type: application/json' --data-binary "$payload" "http://localhost$api_path" >/dev/null
}

phase4c_owned_uid() {
  resource=$1
  name=$2
  namespace=${3:-}
  transaction_id=$4
  if [ -n "$namespace" ]; then
    kubectl get "$resource" "$name" -n "$namespace" -o json |
      jq -er --arg tx "$transaction_id" 'select(.metadata.annotations["blazn.dev/phase4c-transaction"] == $tx) | .metadata.uid'
  else
    kubectl get "$resource" "$name" -o json |
      jq -er --arg tx "$transaction_id" 'select(.metadata.annotations["blazn.dev/phase4c-transaction"] == $tx) | .metadata.uid'
  fi
}

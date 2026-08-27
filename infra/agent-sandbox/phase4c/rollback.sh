#!/bin/sh
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/lib.sh"
[ "$#" -eq 1 ] || { printf 'usage: %s SEALED_TRANSACTION_DIRECTORY\n' "$0" >&2; exit 64; }
transaction=$1
phase4c_require_mutation_authority
phase4c_verify_transaction "$transaction"
pre=$transaction/pre
phase=$(cat "$transaction/phase")
transaction_id=$(sed -n 's/^    blazn.dev\/phase4c-transaction: //p' "$transaction/install.yaml" | head -1)
[ "$(cat "$pre/context")" = "$BLAZN_EXPECTED_CONTEXT" ]
[ "$(cat "$pre/kube-system.uid")" = "$BLAZN_EXPECTED_KUBE_SYSTEM_UID" ]

get_optional_name() {
  resource=$1 name=$2 namespace=${3:-}
  if [ -n "$namespace" ]; then kubectl get "$resource" "$name" -n "$namespace" --ignore-not-found -o name
  else kubectl get "$resource" "$name" --ignore-not-found -o name; fi
}

uid_for_delete() {
  key=$1 resource=$2 name=$3 namespace=$4
  if [ -n "$namespace" ]; then object=$(kubectl get "$resource" "$name" -n "$namespace" --ignore-not-found -o json) || { printf 'rollback lookup failed: %s/%s\n' "$resource" "$name" >&2; return 1; }
  else object=$(kubectl get "$resource" "$name" --ignore-not-found -o json) || { printf 'rollback lookup failed: %s/%s\n' "$resource" "$name" >&2; return 1; }; fi
  [ -n "$object" ] || return 3
  uid=$(printf '%s' "$object" | jq -er --arg tx "$transaction_id" 'select(.metadata.annotations["blazn.dev/phase4c-transaction"] == $tx) | .metadata.uid') || {
    printf 'refusing rollback of an unowned replacement: %s/%s\n' "$resource" "$name" >&2; exit 1
  }
  file=$transaction/uids/$key
  if [ -e "$file" ]; then [ "$(cat "$file")" = "$uid" ] || { printf 'refusing rollback of replaced UID: %s\n' "$key" >&2; exit 1; }
  else printf '%s\n' "$uid" >"$file"; chmod 0600 "$file"; sync -f "$file"; fi
  printf '%s' "$uid"
}

check_namespace_contents() {
  namespace=$1
  unexpected=''
  for resource in $(kubectl api-resources --verbs=list --namespaced -o name | LC_ALL=C sort -u); do
    case "$resource" in events|events.events.k8s.io|pods.metrics.k8s.io) continue ;; esac
    objects=$(kubectl get "$resource" -n "$namespace" --ignore-not-found -o json) || { printf 'namespace rollback inventory lookup failed: %s\n' "$resource" >&2; exit 1; }
    [ -n "$objects" ] || continue
    printf '%s' "$objects" | jq -e --arg tx "$transaction_id" --arg ns "$namespace" '
      [.items[]
       | select(.metadata.annotations["blazn.dev/phase4c-transaction"] != $tx)
       | select(((($ns == "blazn-poc") and (.kind == "ServiceAccount") and (.metadata.name == "default")) or
                 (($ns == "blazn-poc") and (.kind == "ConfigMap") and (.metadata.name == "kube-root-ca.crt")) or
                 (($ns == "agent-sandbox-system") and (.kind == "ServiceAccount") and (.metadata.name == "default")) or
                 (($ns == "agent-sandbox-system") and (.kind == "ConfigMap") and (.metadata.name == "kube-root-ca.crt")) or
                 (($ns == "agent-sandbox-system") and (.kind == "Lease") and (.metadata.name == "a3317529.agent-sandbox.x-k8s.io")) or
                 (($ns == "agent-sandbox-system") and (.kind == "Endpoints") and ((.metadata.name == "agent-sandbox-controller") or (.metadata.name == "agent-sandbox-webhook-service"))) or
                 (($ns == "agent-sandbox-system") and (.kind == "EndpointSlice") and ((.metadata.labels["kubernetes.io/service-name"] == "agent-sandbox-controller") or (.metadata.labels["kubernetes.io/service-name"] == "agent-sandbox-webhook-service")))) | not)]
      | length == 0' >/dev/null || unexpected="$unexpected $resource"
  done
  [ -z "$unexpected" ] || { printf 'refusing namespace deletion with unexpected objects:%s\n' "$unexpected" >&2; exit 1; }
}

case "$phase" in
  sealed) phase4c_write_phase "$transaction" rollback-complete; printf 'No cluster mutation existed; rollback complete\n'; exit 0 ;;
  rollback-complete) printf 'Phase 4C rollback is already complete\n'; exit 0 ;;
  rollback-intent) ;;
  *) phase4c_write_phase "$transaction" rollback-intent ;;
esac

mkdir -p "$transaction/uids"
chmod 0700 "$transaction/uids"
for entry in \
  'sandbox-runner-sa serviceaccount blazn-sandbox-runner blazn-poc' \
  'localqueue localqueue.kueue.x-k8s.io blazn-poc blazn-poc' \
  'controller-role role blazn-agent-sandbox-controller blazn-poc' \
  'controller-binding rolebinding blazn-agent-sandbox-controller blazn-poc' \
  'system-role role blazn-agent-sandbox-system agent-sandbox-system' \
  'system-binding rolebinding blazn-agent-sandbox-system agent-sandbox-system' \
  'controller-sa serviceaccount agent-sandbox-controller agent-sandbox-system' \
  'controller-service service agent-sandbox-controller agent-sandbox-system' \
  'webhook-service service agent-sandbox-webhook-service agent-sandbox-system' \
  'controller-deployment deployment agent-sandbox-controller agent-sandbox-system' \
  'webhook-secret secret agent-sandbox-webhook-certs agent-sandbox-system' \
  'bootstrap-sa serviceaccount blazn-agent-sandbox-ca-bootstrap agent-sandbox-system'; do
  # shellcheck disable=SC2086
  if uid_for_delete $entry >/dev/null; then :
  else lookup_code=$?; [ "$lookup_code" -eq 3 ] || exit "$lookup_code"; fi
done
phase4c_start_uid_proxy "$transaction"
trap 'phase4c_stop_uid_proxy' EXIT HUP INT TERM

delete_if_owned() {
  key=$1 resource=$2 name=$3 namespace=$4 api_path=$5 propagation=${6:-Foreground}
  if uid=$(uid_for_delete "$key" "$resource" "$name" "$namespace"); then :
  else lookup_code=$?; [ "$lookup_code" -eq 3 ] && return 0; return "$lookup_code"; fi
  phase4c_delete_uid "$api_path" "$uid" "$propagation"
}

# Remove an active canary while its controller can still clear finalizers. A
# rollback from canary-intent/canary-ready must not strand the workload
# namespace by stopping reconciliation first.
canary_existed=''
if [ -n "$(kubectl get crd sandboxes.agents.x-k8s.io --ignore-not-found -o name)" ]; then
  canary_existed=$(get_optional_name sandbox phase4c-canary blazn-poc) || { printf 'canary rollback lookup failed\n' >&2; exit 1; }
fi
if [ -n "$canary_existed" ]; then
  delete_if_owned canary-sandbox sandbox phase4c-canary blazn-poc '/apis/agents.x-k8s.io/v1beta1/namespaces/blazn-poc/sandboxes/phase4c-canary' Background
  phase4c_stop_uid_proxy
  kubectl wait --for=delete sandbox/phase4c-canary -n blazn-poc --timeout=120s
  kubectl wait --for=delete pod/phase4c-canary -n blazn-poc --timeout=120s
  kubectl wait --for=delete workload.kueue.x-k8s.io --all -n blazn-poc --timeout=120s
  phase4c_start_uid_proxy "$transaction"
fi

# The controller and Kueue do not copy the transaction annotation to every
# generated object. Scan after the UID-fenced canary cascade so its expected
# Pod/Workload are gone, while any independent unowned object still blocks
# namespace deletion.
blazn_namespace=$(get_optional_name namespace blazn-poc) || { printf 'workload namespace lookup failed\n' >&2; exit 1; }
[ -z "$blazn_namespace" ] || check_namespace_contents blazn-poc
controller_namespace=$(get_optional_name namespace agent-sandbox-system) || { printf 'controller namespace lookup failed\n' >&2; exit 1; }
[ -z "$controller_namespace" ] || check_namespace_contents agent-sandbox-system

# Stop reconciliation only after the active canary and its dependents are gone,
# then remove the uniquely owned namespaces.
delete_if_owned namespace-agent-sandbox-system namespace agent-sandbox-system '' '/api/v1/namespaces/agent-sandbox-system'
controller_namespace=$(get_optional_name namespace agent-sandbox-system) || { printf 'controller namespace post-delete lookup failed\n' >&2; exit 1; }
if [ -n "$controller_namespace" ]; then kubectl wait --for=delete namespace/agent-sandbox-system --timeout=180s; fi
delete_if_owned namespace-blazn-poc namespace blazn-poc '' '/api/v1/namespaces/blazn-poc'
blazn_namespace=$(get_optional_name namespace blazn-poc) || { printf 'workload namespace post-delete lookup failed\n' >&2; exit 1; }
if [ -n "$blazn_namespace" ]; then kubectl wait --for=delete namespace/blazn-poc --timeout=180s; fi
for crd in sandboxclaims.extensions.agents.x-k8s.io sandboxes.agents.x-k8s.io sandboxtemplates.extensions.agents.x-k8s.io sandboxwarmpools.extensions.agents.x-k8s.io; do
  delete_if_owned "crd-$crd" crd "$crd" '' "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/$crd"
done
delete_if_owned observer-binding clusterrolebinding blazn-agent-sandbox-observer '' '/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/blazn-agent-sandbox-observer'
delete_if_owned observer-role clusterrole blazn-agent-sandbox-observer '' '/apis/rbac.authorization.k8s.io/v1/clusterroles/blazn-agent-sandbox-observer'
delete_if_owned admission-binding validatingadmissionpolicybinding blazn-agent-sandbox-boundary '' '/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicybindings/blazn-agent-sandbox-boundary'
delete_if_owned admission-policy validatingadmissionpolicy blazn-agent-sandbox-boundary '' '/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicies/blazn-agent-sandbox-boundary'
# Recover a crash before bootstrap privilege cleanup without broad deletion.
delete_if_owned bootstrap-job job blazn-agent-sandbox-ca-bootstrap agent-sandbox-system '/apis/batch/v1/namespaces/agent-sandbox-system/jobs/blazn-agent-sandbox-ca-bootstrap'
delete_if_owned bootstrap-binding clusterrolebinding blazn-agent-sandbox-ca-bootstrap '' '/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/blazn-agent-sandbox-ca-bootstrap'
delete_if_owned bootstrap-role clusterrole blazn-agent-sandbox-ca-bootstrap '' '/apis/rbac.authorization.k8s.io/v1/clusterroles/blazn-agent-sandbox-ca-bootstrap'
phase4c_stop_uid_proxy
trap - EXIT HUP INT TERM

verify_sealed_target_absent() {
  target=$1 namespace=${2:-}
  if [ -n "$namespace" ]; then
    kubectl wait --for=delete "$target" -n "$namespace" --timeout=120s
    found=$(kubectl get "$target" -n "$namespace" --ignore-not-found -o name)
  else
    kubectl wait --for=delete "$target" --timeout=120s
    found=$(kubectl get "$target" --ignore-not-found -o name)
  fi
  [ -z "$found" ] || { printf 'sealed rollback target remains: %s\n' "$target" >&2; exit 1; }
}
while read -r target namespace; do
  [ -z "$target" ] || verify_sealed_target_absent "$target" "${namespace:-}"
done <"$pre/phase4c-targets"

post=$(mktemp -d "${TMPDIR:-/tmp}/blazn-phase4c-post.XXXXXX")
cleanup() { find "$post" -xdev -type f -delete; find "$post" -xdev -depth -type d -empty -delete; }
trap cleanup EXIT HUP INT TERM
kubectl api-resources -o wide | LC_ALL=C sort >"$post/api-resources.txt"
kubectl get crd -o name >"$post/all-crds.txt"
LC_ALL=C sort "$post/all-crds.txt" | grep -E '(agents\.x-k8s\.io|kueue\.x-k8s\.io)' >"$post/relevant-crds.txt" || :
kubectl get mutatingwebhookconfiguration,validatingwebhookconfiguration,validatingadmissionpolicy,validatingadmissionpolicybinding -o json | jq -S 'del(.metadata.resourceVersion,.items[].metadata.resourceVersion,.items[].metadata.managedFields,.items[].metadata.creationTimestamp,.items[].metadata.uid,.items[].metadata.generation) | .items |= sort_by(.apiVersion,.kind,.metadata.name)' >"$post/admission.json"
kubectl get runtimeclass -o json | jq -S 'del(.metadata.resourceVersion,.metadata.managedFields)' >"$post/runtimeclasses.json"
kubectl get clusterqueue.kueue.x-k8s.io -o json | jq -S 'del(.metadata.resourceVersion,.metadata.managedFields,.items[].metadata.resourceVersion,.items[].metadata.managedFields,.items[].status)' >"$post/clusterqueues.json"
for file in api-resources.txt relevant-crds.txt admission.json runtimeclasses.json clusterqueues.json; do cmp "$pre/$file" "$post/$file" || { printf 'rollback inventory differs: %s\n' "$file" >&2; exit 1; }; done
phase4c_write_phase "$transaction" rollback-complete
printf 'Phase 4C rollback matches preinstall inventory with UID-fenced zero residue\n'

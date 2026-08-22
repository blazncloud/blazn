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

uid_for_delete() {
  key=$1 resource=$2 name=$3 namespace=$4
  if [ -n "$namespace" ]; then object=$(kubectl get "$resource" "$name" -n "$namespace" --ignore-not-found -o json 2>/dev/null || true)
  else object=$(kubectl get "$resource" "$name" --ignore-not-found -o json 2>/dev/null || true); fi
  [ -n "$object" ] || return 1
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
    objects=$(kubectl get "$resource" -n "$namespace" --ignore-not-found -o json)
    [ -n "$objects" ] || continue
    printf '%s' "$objects" | jq -e --arg tx "$transaction_id" --arg ns "$namespace" '
      [.items[] | select(
        (.metadata.annotations["blazn.dev/phase4c-transaction"] != $tx) and
        !(($ns == "blazn-poc") and (.kind == "ServiceAccount") and (.metadata.name == "default")) and
        !(($ns == "blazn-poc") and (.kind == "ConfigMap") and (.metadata.name == "kube-root-ca.crt")) and
        !(($ns == "agent-sandbox-system") and (.kind == "ServiceAccount") and (.metadata.name == "default")) and
        !(($ns == "agent-sandbox-system") and (.kind == "ConfigMap") and (.metadata.name == "kube-root-ca.crt"))
        and !(($ns == "agent-sandbox-system") and (.kind == "Lease") and (.metadata.name == "a3317529.agent-sandbox.x-k8s.io"))
      )] | length == 0' >/dev/null || unexpected="$unexpected $resource"
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
  if uid_for_delete $entry >/dev/null; then :; fi
done
[ -z "$(kubectl get namespace blazn-poc --ignore-not-found -o name)" ] || check_namespace_contents blazn-poc
[ -z "$(kubectl get namespace agent-sandbox-system --ignore-not-found -o name)" ] || check_namespace_contents agent-sandbox-system
phase4c_start_uid_proxy "$transaction"
trap 'phase4c_stop_uid_proxy' EXIT HUP INT TERM

delete_if_owned() {
  key=$1 resource=$2 name=$3 namespace=$4 api_path=$5
  uid=$(uid_for_delete "$key" "$resource" "$name" "$namespace") || return 0
  phase4c_delete_uid "$api_path" "$uid"
}

# Stop reconciliation first, then remove the uniquely owned workload namespace.
delete_if_owned namespace-agent-sandbox-system namespace agent-sandbox-system '' '/api/v1/namespaces/agent-sandbox-system'
if [ -n "$(kubectl get namespace agent-sandbox-system --ignore-not-found -o name)" ]; then kubectl wait --for=delete namespace/agent-sandbox-system --timeout=180s; fi
delete_if_owned namespace-blazn-poc namespace blazn-poc '' '/api/v1/namespaces/blazn-poc'
if [ -n "$(kubectl get namespace blazn-poc --ignore-not-found -o name)" ]; then kubectl wait --for=delete namespace/blazn-poc --timeout=180s; fi
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

post=$(mktemp -d "${TMPDIR:-/tmp}/blazn-phase4c-post.XXXXXX")
cleanup() { find "$post" -xdev -type f -delete; find "$post" -xdev -depth -type d -empty -delete; }
trap cleanup EXIT HUP INT TERM
kubectl api-resources -o wide | LC_ALL=C sort >"$post/api-resources.txt"
kubectl get crd -o name | LC_ALL=C sort | grep -E '(agents\.x-k8s\.io|kueue\.x-k8s\.io)' >"$post/relevant-crds.txt" || :
kubectl get mutatingwebhookconfiguration,validatingwebhookconfiguration,validatingadmissionpolicy,validatingadmissionpolicybinding -o name | LC_ALL=C sort | grep -E '(agent-sandbox|kueue|blazn)' >"$post/relevant-admission.txt" || :
kubectl get runtimeclass -o json | jq -S 'del(.metadata.resourceVersion,.metadata.managedFields)' >"$post/runtimeclasses.json"
kubectl get clusterqueue.kueue.x-k8s.io -o json | jq -S 'del(.metadata.resourceVersion,.metadata.managedFields,.items[].metadata.resourceVersion,.items[].metadata.managedFields,.items[].status)' >"$post/clusterqueues.json"
for file in api-resources.txt relevant-crds.txt relevant-admission.txt runtimeclasses.json clusterqueues.json; do cmp "$pre/$file" "$post/$file" || { printf 'rollback inventory differs: %s\n' "$file" >&2; exit 1; }; done
phase4c_write_phase "$transaction" rollback-complete
printf 'Phase 4C rollback matches preinstall inventory with UID-fenced zero residue\n'

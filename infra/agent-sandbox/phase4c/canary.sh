#!/bin/sh
set -eu
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/lib.sh"
[ "$#" -eq 1 ] || { printf 'usage: %s SEALED_TRANSACTION_DIRECTORY\n' "$0" >&2; exit 64; }
transaction=$1
phase4c_require_mutation_authority
phase4c_verify_transaction "$transaction"
install_bundle=$transaction/install.yaml
fixtures=$transaction/fixtures
pre=$transaction/pre
phase=$(cat "$transaction/phase")
transaction_id=$(sed -n 's/^    blazn.dev\/phase4c-transaction: //p' "$install_bundle" | head -1)
case "$transaction_id" in ????????-????-4???-[89ab]???-????????????) ;; *) printf 'sealed transaction identity is invalid\n' >&2; exit 1 ;; esac
[ "$(cat "$pre/context")" = "$BLAZN_EXPECTED_CONTEXT" ]
[ "$(cat "$pre/kube-system.uid")" = "$BLAZN_EXPECTED_KUBE_SYSTEM_UID" ]
[ "$(kubectl auth whoami -o jsonpath='{.status.userInfo.username}')" = "$(cat "$pre/creator-principal")" ]

record_uid() {
  key=$1 resource=$2 name=$3 namespace=$4
  uid=$(phase4c_owned_uid "$resource" "$name" "$namespace" "$transaction_id")
  file=$transaction/uids/$key
  if [ -e "$file" ]; then [ "$(cat "$file")" = "$uid" ] || { printf 'owned object UID changed: %s\n' "$key" >&2; exit 1; }
  else printf '%s\n' "$uid" >"$file"; chmod 0600 "$file"; sync -f "$file"; fi
}
verify_absent() {
  resource=$1 name=$2
  found=$(kubectl get "$resource" "$name" --ignore-not-found -o name)
  [ -z "$found" ] || { printf 'unowned target appeared before transaction: %s/%s\n' "$resource" "$name" >&2; exit 1; }
}
assert_absent_or_owned() {
  resource=$1 name=$2 namespace=$3
  if [ -n "$namespace" ]; then object=$(kubectl get "$resource" "$name" -n "$namespace" --ignore-not-found -o json)
  else object=$(kubectl get "$resource" "$name" --ignore-not-found -o json); fi
  [ -z "$object" ] || printf '%s' "$object" | jq -e --arg tx "$transaction_id" '.metadata.annotations["blazn.dev/phase4c-transaction"] == $tx' >/dev/null || {
    printf 'refusing to update a replacement object: %s/%s\n' "$resource" "$name" >&2; exit 1
  }
}
mkdir -p "$transaction/uids" "$transaction/evidence"
chmod 0700 "$transaction/uids" "$transaction/evidence"

while :; do
  case "$phase" in
    sealed)
      while IFS= read -r target; do [ -z "$target" ] || verify_absent "${target%%/*}" "${target#*/}"; done <"$pre/phase4c-targets"
      phase4c_write_phase "$transaction" foundation-intent; phase=foundation-intent ;;
    foundation-intent)
      assert_absent_or_owned namespace agent-sandbox-system ''
      assert_absent_or_owned namespace blazn-poc ''
      assert_absent_or_owned serviceaccount blazn-sandbox-runner blazn-poc
      assert_absent_or_owned localqueue.kueue.x-k8s.io blazn-poc blazn-poc
      assert_absent_or_owned clusterrole blazn-agent-sandbox-observer ''
      assert_absent_or_owned clusterrolebinding blazn-agent-sandbox-observer ''
      assert_absent_or_owned role blazn-agent-sandbox-system agent-sandbox-system
      assert_absent_or_owned rolebinding blazn-agent-sandbox-system agent-sandbox-system
      assert_absent_or_owned role blazn-agent-sandbox-controller blazn-poc
      assert_absent_or_owned rolebinding blazn-agent-sandbox-controller blazn-poc
      assert_absent_or_owned validatingadmissionpolicy blazn-agent-sandbox-boundary ''
      assert_absent_or_owned validatingadmissionpolicybinding blazn-agent-sandbox-boundary ''
      kubectl apply --server-side --field-manager="blazn-phase4c-$transaction_id" -f "$fixtures/blazn-poc.yaml" >"$transaction/evidence/apply-namespace.txt"
      kubectl apply --server-side --field-manager="blazn-phase4c-$transaction_id" -f "$fixtures/controller-boundary.yaml" >"$transaction/evidence/apply-boundary.txt"
      record_uid namespace-agent-sandbox-system namespace agent-sandbox-system ''
      record_uid namespace-blazn-poc namespace blazn-poc ''
      record_uid sandbox-runner-sa serviceaccount blazn-sandbox-runner blazn-poc
      record_uid localqueue localqueue.kueue.x-k8s.io blazn-poc blazn-poc
      record_uid observer-role clusterrole blazn-agent-sandbox-observer ''
      record_uid observer-binding clusterrolebinding blazn-agent-sandbox-observer ''
      record_uid system-role role blazn-agent-sandbox-system agent-sandbox-system
      record_uid system-binding rolebinding blazn-agent-sandbox-system agent-sandbox-system
      record_uid controller-role role blazn-agent-sandbox-controller blazn-poc
      record_uid controller-binding rolebinding blazn-agent-sandbox-controller blazn-poc
      record_uid admission-policy validatingadmissionpolicy blazn-agent-sandbox-boundary ''
      record_uid admission-binding validatingadmissionpolicybinding blazn-agent-sandbox-boundary ''
      phase4c_write_phase "$transaction" foundation-applied; phase=foundation-applied ;;
    foundation-applied) phase4c_write_phase "$transaction" controller-intent; phase=controller-intent ;;
    controller-intent)
      for crd in sandboxclaims.extensions.agents.x-k8s.io sandboxes.agents.x-k8s.io sandboxtemplates.extensions.agents.x-k8s.io sandboxwarmpools.extensions.agents.x-k8s.io; do assert_absent_or_owned crd "$crd" ''; done
      assert_absent_or_owned serviceaccount agent-sandbox-controller agent-sandbox-system
      assert_absent_or_owned service agent-sandbox-controller agent-sandbox-system
      assert_absent_or_owned service agent-sandbox-webhook-service agent-sandbox-system
      assert_absent_or_owned deployment agent-sandbox-controller agent-sandbox-system
      kubectl apply --server-side --field-manager="blazn-phase4c-$transaction_id" -f "$install_bundle" >"$transaction/evidence/apply-controller.txt"
      for crd in sandboxclaims.extensions.agents.x-k8s.io sandboxes.agents.x-k8s.io sandboxtemplates.extensions.agents.x-k8s.io sandboxwarmpools.extensions.agents.x-k8s.io; do record_uid "crd-$crd" crd "$crd" ''; done
      record_uid controller-sa serviceaccount agent-sandbox-controller agent-sandbox-system
      record_uid controller-service service agent-sandbox-controller agent-sandbox-system
      record_uid webhook-service service agent-sandbox-webhook-service agent-sandbox-system
      record_uid controller-deployment deployment agent-sandbox-controller agent-sandbox-system
      phase4c_write_phase "$transaction" controller-applied; phase=controller-applied ;;
    controller-applied) phase4c_write_phase "$transaction" bootstrap-intent; phase=bootstrap-intent ;;
    bootstrap-intent)
      assert_absent_or_owned clusterrole blazn-agent-sandbox-ca-bootstrap ''
      assert_absent_or_owned clusterrolebinding blazn-agent-sandbox-ca-bootstrap ''
      assert_absent_or_owned job blazn-agent-sandbox-ca-bootstrap agent-sandbox-system
      assert_absent_or_owned secret agent-sandbox-webhook-certs agent-sandbox-system
      assert_absent_or_owned serviceaccount blazn-agent-sandbox-ca-bootstrap agent-sandbox-system
      kubectl apply --server-side --field-manager="blazn-phase4c-$transaction_id" -f "$fixtures/bootstrap.yaml" >"$transaction/evidence/apply-bootstrap.txt"
      record_uid bootstrap-role clusterrole blazn-agent-sandbox-ca-bootstrap ''
      record_uid bootstrap-binding clusterrolebinding blazn-agent-sandbox-ca-bootstrap ''
      record_uid bootstrap-job job blazn-agent-sandbox-ca-bootstrap agent-sandbox-system
      record_uid webhook-secret secret agent-sandbox-webhook-certs agent-sandbox-system
      record_uid bootstrap-sa serviceaccount blazn-agent-sandbox-ca-bootstrap agent-sandbox-system
      kubectl wait --for=condition=Complete job/blazn-agent-sandbox-ca-bootstrap -n agent-sandbox-system --timeout=120s
      expected_ca=$(kubectl get secret agent-sandbox-webhook-certs -n agent-sandbox-system -o jsonpath='{.data.tls\.crt}')
      for crd in sandboxclaims.extensions.agents.x-k8s.io sandboxes.agents.x-k8s.io sandboxtemplates.extensions.agents.x-k8s.io sandboxwarmpools.extensions.agents.x-k8s.io; do [ "$(kubectl get crd "$crd" -o jsonpath='{.spec.conversion.webhook.clientConfig.caBundle}')" = "$expected_ca" ]; done
      phase4c_write_phase "$transaction" bootstrap-ready; phase=bootstrap-ready ;;
    bootstrap-ready)
      phase4c_start_uid_proxy "$transaction"; trap 'phase4c_stop_uid_proxy' EXIT HUP INT TERM
      [ -z "$(kubectl get job blazn-agent-sandbox-ca-bootstrap -n agent-sandbox-system --ignore-not-found -o name)" ] || phase4c_delete_uid '/apis/batch/v1/namespaces/agent-sandbox-system/jobs/blazn-agent-sandbox-ca-bootstrap' "$(cat "$transaction/uids/bootstrap-job")"
      [ -z "$(kubectl get clusterrolebinding blazn-agent-sandbox-ca-bootstrap --ignore-not-found -o name)" ] || phase4c_delete_uid '/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/blazn-agent-sandbox-ca-bootstrap' "$(cat "$transaction/uids/bootstrap-binding")"
      [ -z "$(kubectl get clusterrole blazn-agent-sandbox-ca-bootstrap --ignore-not-found -o name)" ] || phase4c_delete_uid '/apis/rbac.authorization.k8s.io/v1/clusterroles/blazn-agent-sandbox-ca-bootstrap' "$(cat "$transaction/uids/bootstrap-role")"
      phase4c_stop_uid_proxy; trap - EXIT HUP INT TERM
      kubectl wait --for=delete job/blazn-agent-sandbox-ca-bootstrap -n agent-sandbox-system --timeout=120s
      phase4c_write_phase "$transaction" bootstrap-complete; phase=bootstrap-complete ;;
    bootstrap-complete)
      kubectl wait --for=condition=Established crd/sandboxes.agents.x-k8s.io --timeout=120s
      kubectl wait --for=condition=Available deployment/agent-sandbox-controller -n agent-sandbox-system --timeout=180s
      sa=system:serviceaccount:agent-sandbox-system:agent-sandbox-controller
      [ "$(kubectl auth can-i --as="$sa" delete pods -n blazn-poc)" = yes ]
      [ "$(kubectl auth can-i --as="$sa" delete pods -n default)" = no ]
      [ "$(kubectl auth can-i --as="$sa" patch customresourcedefinitions.apiextensions.k8s.io)" = no ]
      kubectl auth can-i --list --as="$sa" -n blazn-poc >"$transaction/evidence/controller-rbac-blazn-poc.txt"
      kubectl auth can-i --list --as="$sa" -n default >"$transaction/evidence/controller-rbac-default.txt"
      phase4c_write_phase "$transaction" controller-ready; phase=controller-ready ;;
    controller-ready)
      sed 's/namespace: blazn-poc/namespace: default/' "$fixtures/synthetic-canary.yaml" | if kubectl create --dry-run=server -f - >"$transaction/evidence/outside-boundary.txt" 2>&1; then printf 'admission unexpectedly allowed a Sandbox outside blazn-poc\n' >&2; exit 1; fi
      phase4c_write_phase "$transaction" canary-intent; phase=canary-intent ;;
    canary-intent)
      assert_absent_or_owned sandbox phase4c-canary blazn-poc
      kubectl apply --server-side --field-manager="blazn-phase4c-$transaction_id" -f "$fixtures/synthetic-canary.yaml" >"$transaction/evidence/apply-canary.txt"
      record_uid canary-sandbox sandbox phase4c-canary blazn-poc
      kubectl wait --for=condition=Ready sandbox/phase4c-canary -n blazn-poc --timeout=180s
      [ "$(kubectl get pod phase4c-canary -n blazn-poc -o jsonpath='{.status.phase}')" = Running ]
      [ "$(kubectl get workload.kueue.x-k8s.io -n blazn-poc -o jsonpath='{.items[0].status.conditions[?(@.type=="Admitted")].status}')" = True ]
      [ "$(kubectl get workload.kueue.x-k8s.io -n blazn-poc -o jsonpath='{.items[0].status.admission.podSetAssignments[0].resourceUsage.cpu}')" = 100m ]
      [ "$(kubectl get workload.kueue.x-k8s.io -n blazn-poc -o jsonpath='{.items[0].status.admission.podSetAssignments[0].resourceUsage.memory}')" = 64Mi ]
      kubectl get sandbox,pod,workload.kueue.x-k8s.io -n blazn-poc -o yaml >"$transaction/evidence/canary-objects.yaml"
      phase4c_write_phase "$transaction" canary-ready; phase=canary-ready ;;
    canary-ready)
      phase4c_start_uid_proxy "$transaction"; trap 'phase4c_stop_uid_proxy' EXIT HUP INT TERM
      [ -z "$(kubectl get sandbox phase4c-canary -n blazn-poc --ignore-not-found -o name)" ] || phase4c_delete_uid '/apis/agents.x-k8s.io/v1beta1/namespaces/blazn-poc/sandboxes/phase4c-canary' "$(cat "$transaction/uids/canary-sandbox")" Background
      phase4c_stop_uid_proxy; trap - EXIT HUP INT TERM
      kubectl wait --for=delete sandbox/phase4c-canary -n blazn-poc --timeout=120s
      kubectl wait --for=delete pod/phase4c-canary -n blazn-poc --timeout=120s
      kubectl wait --for=delete workload.kueue.x-k8s.io --all -n blazn-poc --timeout=120s
      phase4c_write_phase "$transaction" canary-clean; phase=canary-clean ;;
    canary-clean)
      [ "$(phase4c_count sandbox -n blazn-poc)" -eq 0 ]; [ "$(phase4c_count pod -n blazn-poc)" -eq 0 ]; [ "$(phase4c_count workload.kueue.x-k8s.io -n blazn-poc)" -eq 0 ]
      chmod 0400 "$transaction/evidence"/* "$transaction/uids"/*
      phase4c_write_phase "$transaction" canary-complete
      printf 'Phase 4C synthetic canary transaction completed with zero canary residue\n'; exit 0 ;;
    canary-complete) printf 'Phase 4C canary transaction is already complete\n'; exit 0 ;;
    *) printf 'unknown or rollback transaction phase: %s\n' "$phase" >&2; exit 1 ;;
  esac
done

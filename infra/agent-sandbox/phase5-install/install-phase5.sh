#!/bin/sh
set -eu

# Crash-resumable, UID-fenced installation of the production Agent Sandbox
# controller. Consumes only a sealed transaction directory built from
# render-install-phase5.sh output.
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/../phase4c/lib.sh"
[ "$(id -u)" -eq 0 ] || { printf 'the Phase 5 installation must run as root\n' >&2; exit 1; }
[ "$#" -eq 1 ] || { printf 'usage: %s RENDER_DIRECTORY\n' "$0" >&2; exit 64; }
render_dir=$1
phase4c_require_mutation_authority
: "${BLAZN_PHASE5_TRANSACTION_DIR:?set a durable transaction directory}"
: "${BLAZN_PHASE5_TRANSACTION_ID:?set the transaction UUID used at render time}"
: "${BLAZN_EXPECTED_INSTALL_SHA256:?set the reviewed rendered digest of install.yaml}"
: "${BLAZN_EXPECTED_RBAC_SHA256:?set the reviewed rendered digest of production-rbac.yaml}"
: "${BLAZN_EXPECTED_BOOTSTRAP_SHA256:?set the reviewed rendered digest of bootstrap.yaml}"
command -v jq >/dev/null 2>&1 || { printf 'jq is required\n' >&2; exit 1; }
transaction=$BLAZN_PHASE5_TRANSACTION_DIR
case "$transaction" in /var/lib/blazn/phase5/install-*) ;; *) printf 'install transaction path is outside its reviewed root\n' >&2; exit 1 ;; esac
transaction_name=${transaction#/var/lib/blazn/phase5/}
case "$transaction_name" in */*|*..*|'') printf 'install transaction path must be one clean segment under its reviewed root\n' >&2; exit 1 ;; esac

write_phase() { phase4c_write_phase "$transaction" "$1"; }
object_absent() { discovered=$(kubectl get "$1" "$2" --ignore-not-found -o name) || return 2; [ -z "$discovered" ]; }
owned_uid() {
  if [ "$#" -eq 3 ]; then kubectl get "$1" "$2" -n "$3" -o json; else kubectl get "$1" "$2" -o json; fi |
    jq -er --arg tx "$BLAZN_PHASE5_TRANSACTION_ID" 'select(.metadata.annotations["blazn.dev/phase5-transaction"] == $tx) | .metadata.uid'
}

if [ ! -e "$transaction" ]; then
  for rendered in install.yaml production-rbac.yaml bootstrap.yaml; do
    if ! { [ -f "$render_dir/$rendered" ] && [ ! -L "$render_dir/$rendered" ] && [ "$(stat -c '%h' "$render_dir/$rendered")" = 1 ]; }; then printf 'rendered %s is unsafe\n' "$rendered" >&2; exit 1; fi
  done
  install -d -o root -g root -m 0700 /var/lib/blazn/phase5 2>/dev/null || :
  install -d -o root -g root -m 0700 "$transaction"
  install -o root -g root -m 0400 "$render_dir/install.yaml" "$transaction/install.yaml"
  install -o root -g root -m 0400 "$render_dir/production-rbac.yaml" "$transaction/production-rbac.yaml"
  install -o root -g root -m 0400 "$render_dir/bootstrap.yaml" "$transaction/bootstrap.yaml"
  write_phase sealed
fi
if ! { [ -d "$transaction" ] && [ ! -L "$transaction" ] && [ "$(stat -c '%u:%a' "$transaction")" = 0:700 ]; }; then printf 'install transaction directory is unsafe\n' >&2; exit 1; fi
for sealed_pair in "install.yaml:$BLAZN_EXPECTED_INSTALL_SHA256" "production-rbac.yaml:$BLAZN_EXPECTED_RBAC_SHA256" "bootstrap.yaml:$BLAZN_EXPECTED_BOOTSTRAP_SHA256"; do
  sealed_file=$transaction/${sealed_pair%%:*}
  sealed_digest=${sealed_pair#*:}
  if [ -L "$sealed_file" ] || [ ! -f "$sealed_file" ] || [ "$(stat -c '%u:%a:%h' "$sealed_file")" != 0:400:1 ]; then printf 'sealed input is unsafe: %s\n' "$sealed_file" >&2; exit 1; fi
  [ "$(sha256sum "$sealed_file" | awk '{print $1}')" = "$sealed_digest" ] || { printf 'sealed input digest mismatch: %s\n' "$sealed_file" >&2; exit 1; }
  [ "$(grep -c "blazn.dev/phase5-transaction: $BLAZN_PHASE5_TRANSACTION_ID" "$sealed_file")" -ge 1 ] || { printf 'sealed input does not carry this transaction identity: %s\n' "$sealed_file" >&2; exit 1; }
done
phase=$(cat "$transaction/phase")
case "$phase" in
  complete) printf 'Phase 5 installation transaction is already complete\n'; exit 0 ;;
  rollback-complete) printf 'Phase 5 installation transaction was rolled back; use a new transaction\n' >&2; exit 1 ;;
  sealed|install-intent|install-applied|bootstrap-intent|bootstrap-applied|bootstrap-complete) ;;
  *) printf 'install transaction phase is invalid\n' >&2; exit 1 ;;
esac

boundary_owner=$(kubectl get namespace blazn-poc-sandboxes --ignore-not-found -o jsonpath='{.metadata.annotations.blazn\.dev/phase5-transaction}') || { printf 'boundary namespace discovery failed\n' >&2; exit 1; }
[ -n "$boundary_owner" ] || { printf 'the Phase 5 boundary is not installed\n' >&2; exit 1; }
pod_selector='{"matchExpressions":[{"key":"kubernetes.io/metadata.name","operator":"In","values":["blazn-poc","blazn-poc-sandboxes"]}]}'
kubectl get mutatingwebhookconfiguration kueue-mutating-webhook-configuration -o json | jq -e --argjson selector "$pod_selector" '.webhooks[] | select(.name=="mpod.kb.io") | .namespaceSelector==$selector' >/dev/null || { printf 'the Kueue Pod integration is not live for the reviewed namespaces\n' >&2; exit 1; }

if [ "$phase" = sealed ]; then
  if ! object_absent namespace agent-sandbox-system; then printf 'agent-sandbox-system is present or could not be verified\n' >&2; exit 1; fi
  for pending_crd in sandboxes.agents.x-k8s.io sandboxclaims.extensions.agents.x-k8s.io sandboxtemplates.extensions.agents.x-k8s.io sandboxwarmpools.extensions.agents.x-k8s.io; do
    if ! object_absent crd "$pending_crd"; then printf 'crd %s is present or could not be verified\n' "$pending_crd" >&2; exit 1; fi
  done
  if ! object_absent clusterrole blazn-agent-sandbox-observer; then printf 'the observer ClusterRole is present or could not be verified\n' >&2; exit 1; fi
  write_phase install-intent; phase=install-intent
fi
if [ "$phase" = install-intent ]; then
  if ! object_absent namespace agent-sandbox-system; then
    owned_uid namespace agent-sandbox-system >/dev/null || { printf 'agent-sandbox-system exists without this transaction identity\n' >&2; exit 1; }
  fi
  kubectl apply --server-side --field-manager blazn-phase5-install -f "$transaction/install.yaml" >/dev/null
  kubectl apply --server-side --field-manager blazn-phase5-install -f "$transaction/production-rbac.yaml" >/dev/null
  write_phase install-applied; phase=install-applied
fi
if [ "$phase" = install-applied ]; then
  for established_crd in sandboxes.agents.x-k8s.io sandboxclaims.extensions.agents.x-k8s.io sandboxtemplates.extensions.agents.x-k8s.io sandboxwarmpools.extensions.agents.x-k8s.io; do
    attempt=0
    until kubectl wait --for=condition=Established "crd/$established_crd" --timeout=60s >/dev/null 2>&1; do
      attempt=$((attempt + 1))
      [ "$attempt" -le 20 ] || { printf 'crd %s never became Established\n' "$established_crd" >&2; exit 1; }
      sleep 2
    done
  done
  write_phase bootstrap-intent; phase=bootstrap-intent
fi
if [ "$phase" = bootstrap-intent ]; then
  kubectl apply --server-side --field-manager blazn-phase5-install -f "$transaction/bootstrap.yaml" >/dev/null
  write_phase bootstrap-applied; phase=bootstrap-applied
fi
if [ "$phase" = bootstrap-applied ]; then
  attempt=0
  until [ "$(kubectl get job blazn-agent-sandbox-ca-bootstrap -n agent-sandbox-system -o jsonpath='{.status.succeeded}' 2>/dev/null)" = 1 ]; do
    attempt=$((attempt + 1))
    if [ "$(kubectl get job blazn-agent-sandbox-ca-bootstrap -n agent-sandbox-system -o jsonpath='{.status.failed}' 2>/dev/null)" = 1 ]; then printf 'the CA bootstrap Job failed\n' >&2; exit 1; fi
    [ "$attempt" -le 60 ] || { printf 'the CA bootstrap Job did not complete\n' >&2; exit 1; }
    sleep 3
  done
  expected_ca=$(kubectl get secret agent-sandbox-webhook-certs -n agent-sandbox-system -o jsonpath='{.data.tls\.crt}')
  for patched_crd in sandboxes.agents.x-k8s.io sandboxclaims.extensions.agents.x-k8s.io sandboxtemplates.extensions.agents.x-k8s.io sandboxwarmpools.extensions.agents.x-k8s.io; do
    [ "$(kubectl get crd "$patched_crd" -o jsonpath='{.spec.conversion.webhook.clientConfig.caBundle}')" = "$expected_ca" ] || { printf 'crd %s does not trust the sealed CA\n' "$patched_crd" >&2; exit 1; }
  done
  # The bootstrap privilege is single-use: remove it by recorded UID.
  phase4c_start_uid_proxy "$transaction"
  bootstrap_cr_uid=$(owned_uid clusterrole blazn-agent-sandbox-ca-bootstrap)
  bootstrap_crb_uid=$(owned_uid clusterrolebinding blazn-agent-sandbox-ca-bootstrap)
  bootstrap_sa_uid=$(owned_uid serviceaccount blazn-agent-sandbox-ca-bootstrap agent-sandbox-system)
  phase4c_delete_uid /apis/rbac.authorization.k8s.io/v1/clusterrolebindings/blazn-agent-sandbox-ca-bootstrap "$bootstrap_crb_uid"
  phase4c_delete_uid /apis/rbac.authorization.k8s.io/v1/clusterroles/blazn-agent-sandbox-ca-bootstrap "$bootstrap_cr_uid"
  phase4c_delete_uid /api/v1/namespaces/agent-sandbox-system/serviceaccounts/blazn-agent-sandbox-ca-bootstrap "$bootstrap_sa_uid"
  phase4c_stop_uid_proxy
  write_phase bootstrap-complete; phase=bootstrap-complete
fi
if [ "$phase" = bootstrap-complete ]; then
  attempt=0
  until [ "$(kubectl get deployment agent-sandbox-controller -n agent-sandbox-system -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null)" = True ]; do
    attempt=$((attempt + 1))
    [ "$attempt" -le 60 ] || { printf 'the Agent Sandbox controller never became Available\n' >&2; exit 1; }
    sleep 3
  done
  uids=$transaction/owned-uids.json
  {
    printf '{'
    printf '"namespace/agent-sandbox-system":"%s",' "$(owned_uid namespace agent-sandbox-system)"
    printf '"clusterrole/blazn-agent-sandbox-observer":"%s",' "$(owned_uid clusterrole blazn-agent-sandbox-observer)"
    printf '"clusterrolebinding/blazn-agent-sandbox-observer":"%s",' "$(owned_uid clusterrolebinding blazn-agent-sandbox-observer)"
    printf '"crd/sandboxes.agents.x-k8s.io":"%s",' "$(owned_uid crd sandboxes.agents.x-k8s.io)"
    printf '"crd/sandboxclaims.extensions.agents.x-k8s.io":"%s",' "$(owned_uid crd sandboxclaims.extensions.agents.x-k8s.io)"
    printf '"crd/sandboxtemplates.extensions.agents.x-k8s.io":"%s",' "$(owned_uid crd sandboxtemplates.extensions.agents.x-k8s.io)"
    printf '"crd/sandboxwarmpools.extensions.agents.x-k8s.io":"%s"' "$(owned_uid crd sandboxwarmpools.extensions.agents.x-k8s.io)"
    printf '}\n'
  } >"$uids.tmp"
  jq -e 'to_entries | all(.value | test("^[0-9a-f-]{36}$"))' "$uids.tmp" >/dev/null || { printf 'owned installation identities are incomplete\n' >&2; exit 1; }
  mv "$uids.tmp" "$uids"
  chmod 0600 "$uids"
  write_phase complete
fi
printf 'Phase 5 Agent Sandbox installation complete and verified\n'

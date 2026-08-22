#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
[ "$#" -eq 1 ] || { printf 'usage: %s OUTPUT_DIRECTORY\n' "$0" >&2; exit 64; }
output=$1
[ ! -e "$output" ] || { printf 'refusing to overwrite output directory: %s\n' "$output" >&2; exit 1; }
tmp=$(mktemp -d "${TMPDIR:-/tmp}/blazn-phase4c-fixtures.XXXXXX")
cleanup() {
  find "$tmp" -xdev -type f -delete
  find "$tmp" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM
: "${BLAZN_EXISTING_CLUSTER_QUEUE:?set the reviewed existing ClusterQueue name}"
: "${BLAZN_SYNTHETIC_IMAGE:?set an immutable synthetic image reference}"
: "${BLAZN_PHASE4C_TRANSACTION_ID:?set the reviewed Phase 4C transaction UUID}"
case "$BLAZN_PHASE4C_TRANSACTION_ID" in ????????-????-4???-[89ab]???-????????????) ;; *) printf 'invalid Phase 4C transaction UUID\n' >&2; exit 1 ;; esac
command -v openssl >/dev/null 2>&1 || { printf 'openssl is required\n' >&2; exit 1; }
case "$BLAZN_SYNTHETIC_IMAGE" in *@sha256:*) ;; *) printf 'synthetic image must be digest pinned\n' >&2; exit 1 ;; esac

runtime_trust=qualified-runtime
runtime_line=''
approval='not-applicable'
runtime_admission=''
create_principal=$(kubectl auth whoami -o jsonpath='{.status.userInfo.username}')
[ -n "$create_principal" ] || { printf 'authenticated creator principal is required\n' >&2; exit 1; }
case "$create_principal" in *[!A-Za-z0-9_:@./-]*) printf 'creator principal cannot be safely rendered\n' >&2; exit 1 ;; esac
if [ -n "${BLAZN_RUNTIME_CLASS:-}" ]; then
  case "$BLAZN_RUNTIME_CLASS" in *[!a-z0-9.-]*|'') printf 'invalid RuntimeClass name\n' >&2; exit 1 ;; esac
  : "${BLAZN_EXPECTED_RUNTIME_HANDLER:?set the reviewed RuntimeClass handler}"
  [ "$(kubectl get runtimeclass "$BLAZN_RUNTIME_CLASS" -o jsonpath='{.handler}')" = "$BLAZN_EXPECTED_RUNTIME_HANDLER" ] || {
    printf 'RuntimeClass handler does not match the reviewed capability\n' >&2
    exit 1
  }
  runtime_line="      runtimeClassName: $BLAZN_RUNTIME_CLASS"
  runtime_admission="object.metadata.labels['blazn.dev/runtime-trust'] == 'qualified-runtime' \&\& object.spec.podTemplate.spec.runtimeClassName == '$BLAZN_RUNTIME_CLASS'"
else
  [ "${BLAZN_ORCHESTRATION_ONLY_ACK:-}" = 'approved-non-sensitive-phase4c-canary' ] || {
    printf 'no qualified RuntimeClass; explicit non-sensitive orchestration-only approval is required\n' >&2
    exit 1
  }
  runtime_trust=orchestration-only
  approval=approved
  runtime_admission="object.metadata.labels['blazn.dev/runtime-trust'] == 'orchestration-only' \&\& !has(object.spec.podTemplate.spec.runtimeClassName) \&\& object.metadata.annotations['blazn.dev/non-sensitive-poc'] == 'approved'"
fi

[ "$(kubectl get clusterqueue.kueue.x-k8s.io "$BLAZN_EXISTING_CLUSTER_QUEUE" -o jsonpath='{.status.conditions[?(@.type=="Active")].status}')" = True ] || {
  printf 'existing ClusterQueue is not Active\n' >&2
  exit 1
}
[ -n "$(kubectl get nodes -l blazn.dev/sandbox-eligible=true -o name)" ] || {
  printf 'no existing sandbox-eligible node is available; this prep never labels shared nodes\n' >&2
  exit 1
}
mkdir -m 0700 "$output"
openssl req -x509 -newkey rsa:3072 -sha256 -nodes -days 2 \
  -subj '/CN=agent-sandbox-webhook-service.agent-sandbox-system.svc' \
  -addext 'subjectAltName=DNS:agent-sandbox-webhook-service.agent-sandbox-system.svc,DNS:agent-sandbox-webhook-service.agent-sandbox-system.svc.cluster.local' \
  -keyout "$tmp/webhook.key" -out "$tmp/webhook.crt" >/dev/null 2>&1
chmod 0400 "$tmp/webhook.key" "$tmp/webhook.crt"
cert_b64=$(base64 <"$tmp/webhook.crt" | tr -d '\n')
key_b64=$(base64 <"$tmp/webhook.key" | tr -d '\n')
init_containers=$tmp/bootstrap-init.yaml
: >"$init_containers"
for crd in sandboxclaims.extensions.agents.x-k8s.io sandboxes.agents.x-k8s.io sandboxtemplates.extensions.agents.x-k8s.io sandboxwarmpools.extensions.agents.x-k8s.io; do
  short=$(printf '%s' "$crd" | cut -d. -f1)
  printf '%s\n' \
    "      - name: patch-$short" \
    "        image: registry.k8s.io/kubectl:v1.35.6@sha256:4d8c68e8c2bf360b1808927c4d39fb13998c106cac903622d127a2275c844126" \
    "        args: [\"patch\", \"crd\", \"$crd\", \"--type=merge\", \"-p\", \"{\\\"spec\\\":{\\\"conversion\\\":{\\\"webhook\\\":{\\\"clientConfig\\\":{\\\"caBundle\\\":\\\"$cert_b64\\\"}}}}}\"]" \
    "        resources:" \
    "          requests: {cpu: 10m, memory: 16Mi}" \
    "          limits: {cpu: 100m, memory: 64Mi}" \
    "        securityContext:" \
    "          allowPrivilegeEscalation: false" \
    "          capabilities: {drop: [\"ALL\"]}" \
    "          privileged: false" \
    "          readOnlyRootFilesystem: true" >>"$init_containers"
done
sed "s|BLAZN_EXISTING_CLUSTER_QUEUE|$BLAZN_EXISTING_CLUSTER_QUEUE|g" "$ROOT/blazn-poc.yaml.in" >"$output/blazn-poc.yaml"
sed -e "s|BLAZN_RUNTIME_ADMISSION_EXPRESSION|$runtime_admission|g" -e "s|BLAZN_CREATE_PRINCIPAL|$create_principal|g" -e "s|BLAZN_TRANSACTION_ID|$BLAZN_PHASE4C_TRANSACTION_ID|g" "$ROOT/controller-boundary.yaml.in" >"$output/controller-boundary.yaml"
sed -e "s|BLAZN_TLS_CERT|$cert_b64|g" -e "s|BLAZN_TLS_KEY|$key_b64|g" -e "s|BLAZN_TRANSACTION_ID|$BLAZN_PHASE4C_TRANSACTION_ID|g" "$ROOT/bootstrap.yaml.in" |
  awk -v init="$init_containers" '$0 == "BLAZN_BOOTSTRAP_INIT_CONTAINERS" { while ((getline line < init) > 0) print line; close(init); next } { print }' >"$output/bootstrap.yaml"
sed \
  -e "s|BLAZN_RUNTIME_TRUST|$runtime_trust|g" \
  -e "s|BLAZN_NON_SENSITIVE_APPROVAL|$approval|g" \
  -e "s|BLAZN_RUNTIME_CLASS_LINE|$runtime_line|g" \
  -e "s|BLAZN_SYNTHETIC_IMAGE|$BLAZN_SYNTHETIC_IMAGE|g" \
  -e "s|BLAZN_TRANSACTION_ID|$BLAZN_PHASE4C_TRANSACTION_ID|g" \
  "$ROOT/synthetic-canary.yaml.in" >"$output/synthetic-canary.yaml"
for rendered in "$output"/*.yaml; do
  awk '
    $0 == "metadata:" { inmeta=1; has_annotations=0; print; next }
    inmeta && $0 == "  annotations:" { has_annotations=1; print; print "    blazn.dev/phase4c-transaction: " ENVIRON["BLAZN_PHASE4C_TRANSACTION_ID"]; next }
    inmeta && $0 !~ /^  / { if (!has_annotations) { print "  annotations:"; print "    blazn.dev/phase4c-transaction: " ENVIRON["BLAZN_PHASE4C_TRANSACTION_ID"] }; inmeta=0 }
    { print }
    END { if (inmeta && !has_annotations) { print "  annotations:"; print "    blazn.dev/phase4c-transaction: " ENVIRON["BLAZN_PHASE4C_TRANSACTION_ID"] } }
  ' "$rendered" >"$tmp/annotated.yaml"
  mv "$tmp/annotated.yaml" "$rendered"
done
chmod 0400 "$output"/*.yaml
printf 'Rendered Phase 4C fixtures in %s (%s)\n' "$output" "$runtime_trust"

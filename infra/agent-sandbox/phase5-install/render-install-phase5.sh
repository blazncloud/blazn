#!/bin/sh
set -eu

# Renders the production Agent Sandbox installation: the checksum-locked
# upstream controller bundle (via the reviewed phase4c renderer), the
# production observer/lease RBAC, and the webhook TLS bootstrap with a
# production-lived certificate. Non-mutating.
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
PHASE4C=$ROOT/../phase4c
[ "$#" -eq 1 ] || { printf 'usage: %s OUTPUT_DIRECTORY\n' "$0" >&2; exit 64; }
output=$1
[ ! -e "$output" ] || { printf 'refusing to overwrite output directory\n' >&2; exit 1; }
: "${BLAZN_PHASE5_TRANSACTION_ID:?export one new UUID for this installation transaction}"
case "$BLAZN_PHASE5_TRANSACTION_ID" in ????????-????-4???-[89ab]???-????????????) ;; *) printf 'invalid Phase 5 transaction UUID\n' >&2; exit 1 ;; esac
webhook_cert_days=${BLAZN_WEBHOOK_CERT_DAYS:-397}
case "$webhook_cert_days" in ''|*[!0-9]*|0*) printf 'webhook certificate days must be a positive integer\n' >&2; exit 1 ;; esac
command -v openssl >/dev/null 2>&1 || { printf 'openssl is required\n' >&2; exit 1; }

tmp=$(mktemp -d "${TMPDIR:-/tmp}/blazn-phase5-render.XXXXXX")
cleanup() {
  find "$tmp" -xdev -type f -delete
  find "$tmp" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM
mkdir -m 0700 "$output"

BLAZN_PHASE4C_TRANSACTION_ID=$BLAZN_PHASE5_TRANSACTION_ID "$PHASE4C/render-install.sh" "$tmp/install.yaml"
sed "s|BLAZN_PHASE5_TRANSACTION_ID|$BLAZN_PHASE5_TRANSACTION_ID|g" "$ROOT/production-rbac.yaml.in" >"$tmp/production-rbac.yaml"

openssl req -x509 -newkey rsa:3072 -sha256 -nodes -days "$webhook_cert_days" \
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
sed -e "s|BLAZN_TLS_CERT|$cert_b64|g" -e "s|BLAZN_TLS_KEY|$key_b64|g" -e "s|BLAZN_TRANSACTION_ID|$BLAZN_PHASE5_TRANSACTION_ID|g" "$PHASE4C/bootstrap.yaml.in" |
  awk -v init="$init_containers" '$0 == "BLAZN_BOOTSTRAP_INIT_CONTAINERS" { while ((getline line < init) > 0) print line; close(init); next } { print }' >"$tmp/bootstrap.yaml"

# Every rendered document carries this transaction's identity for UID fencing.
for rendered in "$tmp/install.yaml" "$tmp/production-rbac.yaml" "$tmp/bootstrap.yaml"; do
  BLAZN_PHASE5_TRANSACTION_ID=$BLAZN_PHASE5_TRANSACTION_ID awk '
    $0 == "metadata:" { inmeta=1; has_annotations=0; print; next }
    inmeta && $0 == "  annotations:" { has_annotations=1; print; print "    blazn.dev/phase5-transaction: " ENVIRON["BLAZN_PHASE5_TRANSACTION_ID"]; next }
    inmeta && $0 !~ /^  / { if (!has_annotations) { print "  annotations:"; print "    blazn.dev/phase5-transaction: " ENVIRON["BLAZN_PHASE5_TRANSACTION_ID"] }; inmeta=0 }
    { print }
    END { if (inmeta && !has_annotations) { print "  annotations:"; print "    blazn.dev/phase5-transaction: " ENVIRON["BLAZN_PHASE5_TRANSACTION_ID"] } }
  ' "$rendered" >"$tmp/annotated.yaml"
  mv "$tmp/annotated.yaml" "$rendered"
done
mv "$tmp/install.yaml" "$tmp/production-rbac.yaml" "$tmp/bootstrap.yaml" "$output/"
chmod 0400 "$output"/*.yaml
printf 'Phase 5 installation rendered (webhook certificate valid %s days)\n' "$webhook_cert_days"

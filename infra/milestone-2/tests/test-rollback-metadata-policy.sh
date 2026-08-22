#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
POLICY=$TEST_DIR/../scripts/verify-rollback-metadata.jq
tmp=${TMPDIR:-/tmp}/blazn-rollback-metadata-$$
mkdir "$tmp"
cleanup(){ find "$tmp" -type f -delete; rmdir "$tmp"; }
trap cleanup EXIT HUP INT TERM

digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
jq -cn --arg digest "$digest" '{schemaVersion:"blazn.dev/control-plane-backup/v3",configDigest:$digest,controlApi:{sourceDigest:$digest,image:("blazn-control-api:source-"+($digest|ltrimstr("sha256:"))),imageId:$digest},secretDigests:{"workspace-invitation-hmac-v1":$digest},nodePlanReceiptDigest:$digest}' >"$tmp/v3.json"
jq --arg digest "$digest" '.schemaVersion="blazn.dev/control-plane-backup/v4"|.microk8sIssuerMaterialDigest=$digest' "$tmp/v3.json" >"$tmp/v4.json"

verify(){
  metadata=$1; issuer=${2:-}
  jq -e --arg secretDigest "$digest" --arg sourceDigest "$digest" --arg image "blazn-control-api:source-${digest#sha256:}" --arg imageId "$digest" --arg configDigest "$digest" --arg nodePlanReceiptDigest "$digest" --arg issuerMaterialDigest "$issuer" -f "$POLICY" "$metadata" >/dev/null
}
verify "$tmp/v3.json"
verify "$tmp/v4.json" "$digest"
if verify "$tmp/v3.json" "$digest"; then printf 'issuer-enabled rollback accepted downgrade metadata\n' >&2; exit 1; fi
if verify "$tmp/v4.json"; then printf 'issuer-free rollback accepted issuer metadata\n' >&2; exit 1; fi
jq '.microk8sIssuerMaterialDigest="sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"' "$tmp/v4.json" >"$tmp/mismatch.json"
if verify "$tmp/mismatch.json" "$digest"; then printf 'rollback accepted mismatched issuer metadata\n' >&2; exit 1; fi

trap - EXIT HUP INT TERM
cleanup
printf 'rollback metadata issuer upgrade/downgrade policy passed\n'

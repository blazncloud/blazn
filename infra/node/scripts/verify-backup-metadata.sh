#!/bin/sh
set -eu

die() { printf 'blazn-node-backup: %s\n' "$*" >&2; exit 1; }
[ "$#" -eq 3 ] || die "usage: verify-backup-metadata.sh METADATA OWNERSHIP_RECEIPT KEY_INVENTORY"
metadata=$1; receipt=$2; inventory=$3
for command_name in jq node sha256sum wc; do command -v "$command_name" >/dev/null 2>&1 || die "$command_name is required"; done
for file in "$metadata" "$receipt"; do if [ ! -f "$file" ] || [ -L "$file" ]; then die "required metadata input is unavailable or symlinked: $file"; fi; done
if [ ! -d "$inventory" ] || [ -L "$inventory" ]; then die "Node broker key inventory is unavailable or symlinked"; fi
schema_version=$(jq -er .schemaVersion "$metadata")
case "$schema_version" in blazn.dev/control-plane-backup/v2|blazn.dev/control-plane-backup/v3|blazn.dev/control-plane-backup/v4) ;; *) die "backup metadata version is unsupported" ;; esac
if [ "${BLAZN_NODE_BACKUP_TEST_MODE:-0}" != 1 ]; then
  [ "$(id -u)" -eq 0 ] || die "recoverability verification must run as root"
  [ "$(stat -c '%u:%a' "$inventory")" = 0:700 ] || die "recovery inventory must be root-owned mode 0700"
  protected_names='database-url enrollment-hmac-v1 join-credential-v1'
  if [ "$schema_version" != blazn.dev/control-plane-backup/v2 ]; then protected_names="$protected_names signing-private-v1.b64url signing-public-v1.json node-install-plan-template-v1.json"; fi
  if [ "$schema_version" = blazn.dev/control-plane-backup/v4 ]; then protected_names="$protected_names microk8s-issuer-hmac-v1 microk8s-worker-issuer.json"; fi
  for protected in $protected_names; do
    if [ ! -f "$inventory/$protected" ] || [ -L "$inventory/$protected" ] || [ "$(stat -c '%u:%a' "$inventory/$protected")" != 0:400 ]; then die "recovery inventory entry must be root-owned mode 0400: $protected"; fi
  done
fi

jq -e '
  type=="object" and
  ((.schemaVersion=="blazn.dev/control-plane-backup/v2" and (keys|sort)==["bucket","configDigest","controlApi","correlationId","createdAt","database","fencingToken","nodeBrokerReceiptDigest","schemaVersion","secretDigests"]) or
   (.schemaVersion=="blazn.dev/control-plane-backup/v3" and (keys|sort)==["bucket","configDigest","controlApi","correlationId","createdAt","database","fencingToken","nodeBrokerReceiptDigest","nodePlanReceiptDigest","schemaVersion","secretDigests"]) or
   (.schemaVersion=="blazn.dev/control-plane-backup/v4" and (keys|sort)==["bucket","configDigest","controlApi","correlationId","createdAt","database","fencingToken","microk8sIssuerMaterialDigest","nodeBrokerReceiptDigest","nodePlanReceiptDigest","schemaVersion","secretDigests"])) and
  .database=="blazn" and
  (.correlationId|test("^[A-Za-z0-9._-]+$")) and (.fencingToken|type=="number" and floor==. and .>=1) and
  (.createdAt|test("^[0-9]{8}T[0-9]{6}Z$")) and (.bucket|type=="string" and length>0) and
  (.configDigest|test("^sha256:[a-f0-9]{64}$")) and
  (.controlApi.sourceDigest|test("^sha256:[a-f0-9]{64}$")) and
  (.controlApi.image|test("^blazn-control-api:source-[a-f0-9]{64}$")) and
  (.controlApi.imageId|test("^sha256:[a-f0-9]{64}$")) and
  (.secretDigests|keys==["workspace-invitation-hmac-v1"]) and
  (.secretDigests["workspace-invitation-hmac-v1"]|test("^sha256:[a-f0-9]{64}$")) and
  (.nodeBrokerReceiptDigest|test("^sha256:[a-f0-9]{64}$")) and
  (.schemaVersion=="blazn.dev/control-plane-backup/v2" or (.nodePlanReceiptDigest|test("^sha256:[a-f0-9]{64}$"))) and
  (.schemaVersion!="blazn.dev/control-plane-backup/v4" or (.microk8sIssuerMaterialDigest|test("^sha256:[a-f0-9]{64}$")))' "$metadata" >/dev/null || die "backup metadata does not match the closed v2/v3/v4 schemas"
jq -e '.nodeBroker.schemaVersion=="blazn.dev/node-broker-infra/v1" and .nodeBroker.secretsRoot=="/etc/blazn/node-broker/secrets" and .nodeBroker.databaseRole=="blazn_node_broker"' "$receipt" >/dev/null || die "ownership receipt has no valid Node broker inventory"

receipt_digest=sha256:$(jq -cS .nodeBroker "$receipt" | sha256sum | awk '{print $1}')
[ "$(jq -er .nodeBrokerReceiptDigest "$metadata")" = "$receipt_digest" ] || die "backup metadata Node broker receipt digest mismatch"
if [ "$schema_version" != blazn.dev/control-plane-backup/v2 ]; then
  node_plan_receipt_digest=sha256:$(jq -cS .nodePlan "$receipt" | sha256sum | awk '{print $1}')
  [ "$(jq -er .nodePlanReceiptDigest "$metadata")" = "$node_plan_receipt_digest" ] || die "backup metadata Node plan receipt digest mismatch"
fi
for name in database-url enrollment-hmac-v1 join-credential-v1; do
  file=$inventory/$name
  if [ ! -f "$file" ] || [ -L "$file" ]; then die "Node broker key inventory entry is missing or symlinked: $name"; fi
  expected=$(jq -er --arg name "$name" '.nodeBroker.digests[$name]' "$receipt")
  [ "$expected" = "sha256:$(sha256sum "$file" | awk '{print $1}')" ] || die "Node broker key inventory digest mismatch: $name"
done
[ "$(wc -c <"$inventory/enrollment-hmac-v1" | tr -d ' ')" = 32 ] || die "enrollment HMAC inventory is not 32 bytes"
[ "$(wc -c <"$inventory/join-credential-v1" | tr -d ' ')" = 32 ] || die "join credential inventory is not 32 bytes"
if [ "$schema_version" != blazn.dev/control-plane-backup/v2 ]; then
  for name in signing-private-v1.b64url signing-public-v1.json node-install-plan-template-v1.json; do
    if [ ! -f "$inventory/$name" ] || [ -L "$inventory/$name" ]; then die "separately protected Node plan recovery entry is missing or symlinked: $name"; fi
  done
  BLAZN_NODE_PLAN_ROOT="$inventory" \
  NODE_PLAN_SIGNING_PRIVATE_KEY_FILE="$inventory/signing-private-v1.b64url" \
  BLAZN_NODE_PLAN_SOURCE_TEMPLATES="$(CDPATH='' cd -- "$(dirname -- "$0")/../templates" && pwd)" \
    node "$(dirname -- "$0")/verify-plan-materials.mjs" >/dev/null
  [ "$(jq -er .nodePlan.publicKeyFingerprint "$receipt")" = "$(jq -er .publicKeyFingerprint "$inventory/signing-public-v1.json")" ] || die "Node plan recovery fingerprint mismatch"
  [ "$(jq -er .nodePlan.templateDigest "$receipt")" = "sha256:$(sha256sum "$inventory/node-install-plan-template-v1.json" | awk '{print $1}')" ] || die "Node plan recovery template mismatch"
fi
if [ "$schema_version" = blazn.dev/control-plane-backup/v4 ]; then
  issuer_receipt=$inventory/microk8s-worker-issuer.json; issuer_key=$inventory/microk8s-issuer-hmac-v1
  issuer_material=$(jq -cS '{binary,config,unit,tmpfiles,environment,secret,socket,microk8s,recovery,brokerUid,liveJoinBlocked}' "$issuer_receipt")
  issuer_digest=sha256:$(printf '%s' "$issuer_material" | sha256sum | awk '{print $1}')
  [ "$issuer_digest" = "$(jq -er .microk8sIssuer.materialDigest "$receipt")" ] || die "ownership receipt issuer material digest mismatch"
  [ "$issuer_digest" = "$(jq -er .microk8sIssuerMaterialDigest "$metadata")" ] || die "backup metadata issuer material digest mismatch"
  [ "sha256:$(sha256sum "$issuer_key" | awk '{print $1}')" = "$(jq -er .secret.digest "$issuer_receipt")" ] || die "issuer HMAC recovery digest mismatch"
  if [ "$(wc -c <"$issuer_key" | tr -d ' ')" != 43 ] || ! grep -Eq '^[A-Za-z0-9_-]{43}$' "$issuer_key"; then die "issuer HMAC recovery encoding is invalid"; fi
fi
printf 'backup metadata and separately recoverable Node broker and plan key inventories verified\n'

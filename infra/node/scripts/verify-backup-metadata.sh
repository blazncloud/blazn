#!/bin/sh
set -eu

die() { printf 'blazn-node-backup: %s\n' "$*" >&2; exit 1; }
[ "$#" -eq 3 ] || die "usage: verify-backup-metadata.sh METADATA OWNERSHIP_RECEIPT KEY_INVENTORY"
metadata=$1; receipt=$2; inventory=$3
for command_name in jq sha256sum wc; do command -v "$command_name" >/dev/null 2>&1 || die "$command_name is required"; done
for file in "$metadata" "$receipt"; do if [ ! -f "$file" ] || [ -L "$file" ]; then die "required metadata input is unavailable or symlinked: $file"; fi; done
if [ ! -d "$inventory" ] || [ -L "$inventory" ]; then die "Node broker key inventory is unavailable or symlinked"; fi

jq -e '
  type=="object" and (keys|sort)==["bucket","configDigest","controlApi","correlationId","createdAt","database","fencingToken","nodeBrokerReceiptDigest","schemaVersion","secretDigests"] and
  .schemaVersion=="blazn.dev/control-plane-backup/v2" and .database=="blazn" and
  (.correlationId|test("^[A-Za-z0-9._-]+$")) and (.fencingToken|type=="number" and floor==. and .>=1) and
  (.createdAt|test("^[0-9]{8}T[0-9]{6}Z$")) and (.bucket|type=="string" and length>0) and
  (.configDigest|test("^sha256:[a-f0-9]{64}$")) and
  (.controlApi.sourceDigest|test("^sha256:[a-f0-9]{64}$")) and
  (.controlApi.image|test("^blazn-control-api:source-[a-f0-9]{64}$")) and
  (.controlApi.imageId|test("^sha256:[a-f0-9]{64}$")) and
  (.secretDigests|keys==["workspace-invitation-hmac-v1"]) and
  (.secretDigests["workspace-invitation-hmac-v1"]|test("^sha256:[a-f0-9]{64}$")) and
  (.nodeBrokerReceiptDigest|test("^sha256:[a-f0-9]{64}$"))' "$metadata" >/dev/null || die "backup metadata does not match the closed v2 schema"
jq -e '.nodeBroker.schemaVersion=="blazn.dev/node-broker-infra/v1" and .nodeBroker.secretsRoot=="/etc/blazn/node-broker/secrets" and .nodeBroker.databaseRole=="blazn_node_broker"' "$receipt" >/dev/null || die "ownership receipt has no valid Node broker inventory"

receipt_digest=sha256:$(jq -cS .nodeBroker "$receipt" | sha256sum | awk '{print $1}')
[ "$(jq -er .nodeBrokerReceiptDigest "$metadata")" = "$receipt_digest" ] || die "backup metadata Node broker receipt digest mismatch"
for name in database-url enrollment-hmac-v1 join-credential-v1; do
  file=$inventory/$name
  if [ ! -f "$file" ] || [ -L "$file" ]; then die "Node broker key inventory entry is missing or symlinked: $name"; fi
  expected=$(jq -er --arg name "$name" '.nodeBroker.digests[$name]' "$receipt")
  [ "$expected" = "sha256:$(sha256sum "$file" | awk '{print $1}')" ] || die "Node broker key inventory digest mismatch: $name"
done
[ "$(wc -c <"$inventory/enrollment-hmac-v1" | tr -d ' ')" = 32 ] || die "enrollment HMAC inventory is not 32 bytes"
[ "$(wc -c <"$inventory/join-credential-v1" | tr -d ' ')" = 32 ] || die "join credential inventory is not 32 bytes"
printf 'backup metadata and recoverable Node broker key inventory verified\n'

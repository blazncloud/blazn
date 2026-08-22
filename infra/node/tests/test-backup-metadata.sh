#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
VERIFY=$TEST_DIR/../scripts/verify-backup-metadata.sh
tmp=${TMPDIR:-/tmp}/blazn-node-backup-metadata-$$
mkdir "$tmp" "$tmp/keys"
cleanup() { find "$tmp" -type f -delete; find "$tmp" -depth -type d -empty -delete; }
trap cleanup EXIT HUP INT TERM
printf 'postgresql://blazn_node_broker:%064d@postgres:5432/blazn\n' 1 >"$tmp/keys/database-url"
dd if=/dev/zero of="$tmp/keys/enrollment-hmac-v1" bs=32 count=1 status=none
dd if=/dev/zero of="$tmp/keys/join-credential-v1" bs=32 count=1 status=none
printf '{"phase":"published"}\n' >"$tmp/creation-journal.json"
node=$(jq -cnS \
  --arg database "sha256:$(sha256sum "$tmp/keys/database-url" | awk '{print $1}')" \
  --arg enrollment "sha256:$(sha256sum "$tmp/keys/enrollment-hmac-v1" | awk '{print $1}')" \
  --arg join "sha256:$(sha256sum "$tmp/keys/join-credential-v1" | awk '{print $1}')" \
  --arg journal "sha256:$(sha256sum "$tmp/creation-journal.json" | awk '{print $1}')" \
  '{schemaVersion:"blazn.dev/node-broker-infra/v1",secretsRoot:"/etc/blazn/node-broker/secrets",databaseRole:"blazn_node_broker",keyIds:{enrollment:"node-enrollment/v1",joinCredential:"node-join-credential/v1"},digests:{"database-url":$database,"enrollment-hmac-v1":$enrollment,"join-credential-v1":$join},creationJournal:{path:"/var/lib/blazn/ownership/node-broker-secret-create.json",digest:$journal}}')
jq -cn --argjson node "$node" '{nodeBroker:$node}' >"$tmp/receipt.json"
digest=sha256:$(jq -cS .nodeBroker "$tmp/receipt.json" | sha256sum | awk '{print $1}')
jq -cn --arg digest "$digest" '{schemaVersion:"blazn.dev/control-plane-backup/v2",correlationId:"test",fencingToken:1,createdAt:"20260822T080000Z",database:"blazn",bucket:"blazn-poc",configDigest:("sha256:"+("1"*64)),controlApi:{sourceDigest:("sha256:"+("2"*64)),image:("blazn-control-api:source-"+("2"*64)),imageId:("sha256:"+("3"*64))},secretDigests:{"workspace-invitation-hmac-v1":("sha256:"+("4"*64))},nodeBrokerReceiptDigest:$digest}' >"$tmp/metadata.json"
"$VERIFY" "$tmp/metadata.json" "$tmp/receipt.json" "$tmp/keys" >/dev/null

jq '.nodeBrokerReceiptDigest="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"' "$tmp/metadata.json" >"$tmp/bad-metadata.json"
if "$VERIFY" "$tmp/bad-metadata.json" "$tmp/receipt.json" "$tmp/keys" >"$tmp/out" 2>"$tmp/err"; then printf 'metadata digest mismatch unexpectedly passed\n' >&2; exit 1; fi
grep -F 'receipt digest mismatch' "$tmp/err" >/dev/null
printf x >>"$tmp/keys/join-credential-v1"
if "$VERIFY" "$tmp/metadata.json" "$tmp/receipt.json" "$tmp/keys" >"$tmp/out" 2>"$tmp/err"; then printf 'key inventory mismatch unexpectedly passed\n' >&2; exit 1; fi
grep -F 'key inventory digest mismatch' "$tmp/err" >/dev/null
trap - EXIT HUP INT TERM
cleanup
printf 'backup metadata and recoverability mismatch tests passed\n'

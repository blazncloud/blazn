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
printf '%s\n' AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA >"$tmp/keys/signing-private-v1.b64url"
node - "$tmp/keys/signing-public-v1.json" <<'EOF'
const {createHash,createPrivateKey}=require("node:crypto"),fs=require("node:fs");
const seed=Buffer.alloc(32),key=createPrivateKey({key:Buffer.concat([Buffer.from("302e020100300506032b657004220420","hex"),seed]),format:"der",type:"pkcs8"}),x=key.export({format:"jwk"}).x;
fs.writeFileSync(process.argv[2],JSON.stringify({schemaVersion:"blazn.dev/node-plan-signing-key/v1",keyId:"control-plane-node-plan/v1",publicKey:x,publicKeyFingerprint:`sha256:${createHash("sha256").update(Buffer.from(x,"base64url")).digest("hex")}`}));
EOF
cp "$TEST_DIR/../templates/node-install-plan-template-v1.json" "$tmp/keys/node-install-plan-template-v1.json"
printf '{"phase":"published"}\n' >"$tmp/creation-journal.json"
node=$(jq -cnS \
  --arg database "sha256:$(sha256sum "$tmp/keys/database-url" | awk '{print $1}')" \
  --arg enrollment "sha256:$(sha256sum "$tmp/keys/enrollment-hmac-v1" | awk '{print $1}')" \
  --arg join "sha256:$(sha256sum "$tmp/keys/join-credential-v1" | awk '{print $1}')" \
  --arg journal "sha256:$(sha256sum "$tmp/creation-journal.json" | awk '{print $1}')" \
  '{schemaVersion:"blazn.dev/node-broker-infra/v1",secretsRoot:"/etc/blazn/node-broker/secrets",databaseRole:"blazn_node_broker",keyIds:{enrollment:"node-enrollment/v1",joinCredential:"node-join-credential/v1"},digests:{"database-url":$database,"enrollment-hmac-v1":$enrollment,"join-credential-v1":$join},creationJournal:{path:"/var/lib/blazn/ownership/node-broker-secret-create.json",digest:$journal}}')
plan=$(jq -cn --arg fingerprint "$(jq -r .publicKeyFingerprint "$tmp/keys/signing-public-v1.json")" --arg templateDigest "sha256:$(sha256sum "$tmp/keys/node-install-plan-template-v1.json"|awk '{print $1}')" '{schemaVersion:"blazn.dev/node-plan-material/v1",root:"/etc/blazn/node-plan",keyId:"control-plane-node-plan/v1",publicKeyFingerprint:$fingerprint,templateId:"frontro-poc-worker/v1",templateDigest:$templateDigest,creationJournal:{path:"/var/lib/blazn/ownership/node-plan-material-create.json",digest:("sha256:"+("a"*64))}}')
jq -cn --argjson node "$node" --argjson plan "$plan" '{nodeBroker:$node,nodePlan:$plan}' >"$tmp/receipt.json"
digest=sha256:$(jq -cS .nodeBroker "$tmp/receipt.json" | sha256sum | awk '{print $1}')
plan_digest=sha256:$(jq -cS .nodePlan "$tmp/receipt.json" | sha256sum | awk '{print $1}')
jq -cn --arg digest "$digest" --arg planDigest "$plan_digest" '{schemaVersion:"blazn.dev/control-plane-backup/v2",correlationId:"test",fencingToken:1,createdAt:"20260822T080000Z",database:"blazn",bucket:"blazn-poc",configDigest:("sha256:"+("1"*64)),controlApi:{sourceDigest:("sha256:"+("2"*64)),image:("blazn-control-api:source-"+("2"*64)),imageId:("sha256:"+("3"*64))},secretDigests:{"workspace-invitation-hmac-v1":("sha256:"+("4"*64))},nodeBrokerReceiptDigest:$digest,nodePlanReceiptDigest:$planDigest}' >"$tmp/metadata.json"
BLAZN_NODE_BACKUP_TEST_MODE=1 "$VERIFY" "$tmp/metadata.json" "$tmp/receipt.json" "$tmp/keys" >/dev/null

jq '.nodeBrokerReceiptDigest="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"' "$tmp/metadata.json" >"$tmp/bad-metadata.json"
if BLAZN_NODE_BACKUP_TEST_MODE=1 "$VERIFY" "$tmp/bad-metadata.json" "$tmp/receipt.json" "$tmp/keys" >"$tmp/out" 2>"$tmp/err"; then printf 'metadata digest mismatch unexpectedly passed\n' >&2; exit 1; fi
grep -F 'receipt digest mismatch' "$tmp/err" >/dev/null
printf x >>"$tmp/keys/join-credential-v1"
if BLAZN_NODE_BACKUP_TEST_MODE=1 "$VERIFY" "$tmp/metadata.json" "$tmp/receipt.json" "$tmp/keys" >"$tmp/out" 2>"$tmp/err"; then printf 'key inventory mismatch unexpectedly passed\n' >&2; exit 1; fi
grep -F 'key inventory digest mismatch' "$tmp/err" >/dev/null
trap - EXIT HUP INT TERM
cleanup
printf 'backup metadata and recoverability mismatch tests passed\n'

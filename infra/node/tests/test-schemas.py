#!/usr/bin/env python3
import json
import pathlib
import sys

try:
    import jsonschema
except ImportError:
    print("Node schema semantic validation skipped: python jsonschema unavailable")
    raise SystemExit(0)

root = pathlib.Path(__file__).resolve().parents[1]
m2 = root.parent / "milestone-2"
node = json.loads((root / "node-broker-receipt.schema.json").read_text())
upgrade = json.loads((root / "node-broker-upgrade-receipt.schema.json").read_text())
ownership = json.loads((m2 / "ownership-receipt.schema.json").read_text())
metadata = json.loads((m2 / "backup-metadata.schema.json").read_text())
for schema in (node, upgrade, ownership, metadata):
    jsonschema.Draft202012Validator.check_schema(schema)

digest = "sha256:" + "a" * 64
node_value = {
    "schemaVersion": "blazn.dev/node-broker-infra/v1",
    "secretsRoot": "/etc/blazn/node-broker/secrets",
    "databaseRole": "blazn_node_broker",
    "keyIds": {"enrollment": "node-enrollment/v1", "joinCredential": "node-join-credential/v1"},
    "digests": {"database-url": digest, "enrollment-hmac-v1": digest, "join-credential-v1": digest},
    "creationJournal": {"path": "/var/lib/blazn/ownership/node-broker-upgrade-secret-create.json", "digest": digest},
}
store = {node["$id"]: node}
resolver = jsonschema.RefResolver.from_schema(upgrade, store=store)
jsonschema.Draft202012Validator(upgrade, resolver=resolver).validate({
    "schemaVersion": "blazn.dev/node-broker-upgrade/v2",
    "owner": "blazn-poc",
    "host": "test",
    "phase": "complete",
    "createdAt": "2026-08-22T08:00:00Z",
    "inputs": {
        "mainReceipt": {"path": "/a", "backupPath": "/b", "digest": digest},
        "environment": {"path": "/c", "backupPath": "/d", "digest": digest},
        "buildReceipt": {"path": "/e", "backupPath": "/f", "digest": "", "present": False},
        "sourceDigest": "",
        "configDigest": digest,
    },
    "nodeBroker": node_value,
})
jsonschema.Draft202012Validator(metadata).validate({
    "schemaVersion": "blazn.dev/control-plane-backup/v2",
    "correlationId": "test",
    "fencingToken": 1,
    "createdAt": "20260822T080000Z",
    "database": "blazn",
    "bucket": "blazn-poc",
    "configDigest": digest,
    "controlApi": {
        "sourceDigest": digest,
        "image": "blazn-control-api:source-" + "a" * 64,
        "imageId": digest,
    },
    "secretDigests": {"workspace-invitation-hmac-v1": digest},
    "nodeBrokerReceiptDigest": digest,
})
print("Node JSON Schemas and external references validated")

#!/usr/bin/env python3
import copy
import json
import pathlib

try:
    import jsonschema
except ImportError:
    print("Node plan template schema rendering skipped: python jsonschema unavailable")
    raise SystemExit(0)

root = pathlib.Path(__file__).resolve().parents[1]
repo = root.parents[1]
bundle = json.loads((root / "templates" / "node-install-plan-template-v1.json").read_text())
plan_schema = json.loads((repo / "packages" / "contracts" / "nodes" / "node-install-plan.schema.json").read_text())
validator = jsonschema.Draft202012Validator(plan_schema, format_checker=jsonschema.FormatChecker())
dynamic = {
    "schemaVersion": "nodes/v1alpha1",
    "planId": "11111111-1111-4111-8111-111111111111",
    "nodeId": "22222222-2222-4222-8222-222222222222",
    "enrollmentId": "33333333-3333-4333-8333-333333333333",
    "workspaceId": "44444444-4444-4444-8444-444444444444",
    "idempotencyKey": "template-test",
    "approvedBy": "55555555-5555-4555-8555-555555555555",
    "approvedAt": "2026-08-22T12:00:00Z",
    "hostname": "node.example.test",
    "issuedAt": "2026-08-22T12:00:00Z",
    "expiresAt": "2026-08-22T12:15:00Z",
    "signingKeyId": "control-plane-node-plan/v1",
    "digest": "sha256:" + "a" * 64,
    "signature": "A" * 86,
}
cases = {
    "ubuntu-26.04-amd64-worker/v1": ("fresh", "linux", "amd64"),
    "existing-linux-worker-adopt/v1": ("adopt", "linux", "amd64"),
    "macos-lima-worker-adopt/v1": ("adopt", "macos", "arm64"),
}
for profile_id, (mode, platform, architecture) in cases.items():
    rendered = copy.deepcopy(bundle["profiles"][profile_id])
    rendered.update(dynamic)
    rendered.update({
        "mode": mode,
        "installProfile": profile_id,
        "target": {
            "platform": platform,
            "architecture": architecture,
            "machineFingerprint": "b" * 64,
            "nodePublicKeyFingerprint": "sha256:" + "c" * 64,
            "minCpu": 1,
            "minMemoryBytes": 1073741824,
            "minDiskBytes": 10737418240,
        },
    })
    errors = sorted(validator.iter_errors(rendered), key=lambda error: list(error.path))
    if errors:
        detail = "\n".join(f"{list(error.path)}: {error.message}" for error in errors)
        raise AssertionError(f"{profile_id} does not render to NodeInstallPlan:\n{detail}")
print("all frozen Node plan profiles render against NodeInstallPlan")

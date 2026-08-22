#!/usr/bin/env python3
import copy
import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import textwrap

try:
    import jsonschema
except ImportError:
    print("Node plan template schema rendering skipped: python jsonschema unavailable")
    raise SystemExit(0)

root = pathlib.Path(__file__).resolve().parents[1]
repo = root.parents[1]
bundle = json.loads((root / "templates" / "node-install-plan-template-v1.json").read_text())
template_schema = json.loads((root / "node-install-plan-template.schema.json").read_text())
plan_schema = json.loads((repo / "packages" / "contracts" / "nodes" / "node-install-plan.schema.json").read_text())
format_checker = jsonschema.FormatChecker()
template_validator = jsonschema.Draft202012Validator(template_schema, format_checker=format_checker)
plan_validator = jsonschema.Draft202012Validator(plan_schema, format_checker=format_checker)

profile_fields = (
    "cluster",
    "registryTrust",
    "components",
    "nodeService",
    "labels",
    "taints",
    "resourceBounds",
    "mutations",
    "validationTests",
    "rollback",
)
for field in profile_fields:
    if template_schema["$defs"][field] != plan_schema["properties"][field]:
        raise AssertionError(f"template $defs/{field} drifted from NodeInstallPlan.properties.{field}")
template_validator.validate(bundle)


def assert_template_rejected(value, label):
    if template_validator.is_valid(value):
        raise AssertionError(f"template schema accepted {label}")


not_closed = copy.deepcopy(bundle)
not_closed["profiles"]["ubuntu-26.04-amd64-worker/v1"]["cluster"]["unexpected"] = True
assert_template_rejected(not_closed, "an unknown nested cluster field")
missing_source_class = copy.deepcopy(bundle)
del missing_source_class["profiles"]["ubuntu-26.04-amd64-worker/v1"]["components"][0]["sourceClass"]
assert_template_rejected(missing_source_class, "a component without sourceClass")
extra_profile = copy.deepcopy(bundle)
extra_profile["profiles"]["unexpected/v1"] = copy.deepcopy(
    extra_profile["profiles"]["existing-linux-worker-adopt/v1"]
)
assert_template_rejected(extra_profile, "a fourth profile")

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
rendered_plans = []
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
    errors = sorted(plan_validator.iter_errors(rendered), key=lambda error: list(error.path))
    if errors:
        detail = "\n".join(f"{list(error.path)}: {error.message}" for error in errors)
        raise AssertionError(f"{profile_id} does not render to NodeInstallPlan:\n{detail}")
    rendered_plans.append({"name": profile_id, "plan": rendered, "wantValid": True})

invalid_semantics = copy.deepcopy(rendered_plans[0]["plan"])
invalid_semantics["mutations"] = [
    mutation for mutation in invalid_semantics["mutations"]
    if not (mutation["kind"] == "systemd_unit" and mutation["action"] == "enable")
]
rendered_plans.append({
    "name": "fresh profile without service enable",
    "plan": invalid_semantics,
    "wantValid": False,
})

go_test_source = textwrap.dedent(
    """\
    package client

    import (
        "bytes"
        "encoding/json"
        "os"
        "testing"
    )

    type testCase struct {
        Name      string          `json:"name"`
        Plan      json.RawMessage `json:"plan"`
        WantValid bool            `json:"wantValid"`
    }

    func TestRenderedPlans(t *testing.T) {
        var cases []testCase
        input, err := os.Open("cases.json")
        if err != nil { t.Fatal(err) }
        defer input.Close()
        if err := json.NewDecoder(input).Decode(&cases); err != nil {
            t.Fatal(err)
        }
        for _, test := range cases {
            _, err := DecodeNodeInstallPlan(bytes.NewReader(test.Plan))
            if test.WantValid && err != nil {
                t.Fatalf("%s rejected by generated validator: %v", test.Name, err)
            }
            if !test.WantValid && err == nil {
                t.Fatalf("%s accepted by generated validator", test.Name)
            }
        }
    }
    """
)
go_env = os.environ.copy()
go_env.update({
    "GOPROXY": "off",
    "GOSUMDB": "off",
    "GOTOOLCHAIN": "local",
    "GOWORK": "off",
    "GOFLAGS": "-mod=readonly",
})
with tempfile.TemporaryDirectory(prefix=".node-plan-validator-", dir=repo) as temp_dir:
    module = pathlib.Path(temp_dir)
    client_dir = module / "client"
    jcs_dir = module / "jcs"
    client_dir.mkdir()
    jcs_dir.mkdir()
    (module / "go.mod").write_text('module node-plan-validator\n\ngo 1.24\n\nrequire github.com/gowebpki/jcs v0.0.0\nreplace github.com/gowebpki/jcs => ./jcs\n')
    (jcs_dir / "go.mod").write_text('module github.com/gowebpki/jcs\n\ngo 1.24\n')
    (jcs_dir / "jcs.go").write_text('package jcs\nfunc Transform(value []byte) ([]byte, error) { panic("canonicalization is outside this validator-only gate") }\n')
    shutil.copy2(repo / "internal" / "client" / "node.gen.go", client_dir / "node.gen.go")
    (client_dir / "node_validator_test.go").write_text(go_test_source)
    (client_dir / "cases.json").write_text(json.dumps(rendered_plans))
    result = subprocess.run(
        ["go", "test", "./client"],
        cwd=module,
        env=go_env,
        text=True,
        capture_output=True,
        check=False,
    )
if result.returncode != 0:
    raise AssertionError(
        "generated Node API validator gate failed with network access disabled:\n"
        + result.stdout
        + result.stderr
    )
print("all frozen Node plan profiles pass the closed schema and generated API validator")

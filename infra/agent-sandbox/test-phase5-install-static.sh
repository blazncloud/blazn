#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
INSTALL=$ROOT/phase5-install
tmp=$(mktemp -d "${TMPDIR:-/tmp}/blazn-phase5-install-static.XXXXXX")
cleanup() {
  find "$tmp" -xdev -type f -delete
  find "$tmp" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM

for script in "$INSTALL"/*.sh; do sh -n "$script"; done
python3 -c 'import yaml' 2>/dev/null || { printf 'python3 yaml module is required\n' >&2; exit 1; }
command -v openssl >/dev/null 2>&1 || { printf 'openssl is required\n' >&2; exit 1; }

if BLAZN_PHASE5_TRANSACTION_ID=not-a-uuid "$INSTALL/render-install-phase5.sh" "$tmp/reject" 2>/dev/null; then exit 1; fi
if BLAZN_PHASE5_TRANSACTION_ID=99999999-9999-4999-8999-999999999999 BLAZN_WEBHOOK_CERT_DAYS=0 "$INSTALL/render-install-phase5.sh" "$tmp/reject" 2>/dev/null; then exit 1; fi
BLAZN_PHASE5_TRANSACTION_ID=99999999-9999-4999-8999-999999999999 "$INSTALL/render-install-phase5.sh" "$tmp/render" >/dev/null

python3 - "$tmp/render" <<'PY'
import base64, subprocess, sys, yaml
render = sys.argv[1]
tx = "99999999-9999-4999-8999-999999999999"

install = [d for d in yaml.safe_load_all(open(f"{render}/install.yaml")) if d]
kinds = [d["kind"] for d in install]
assert kinds.count("CustomResourceDefinition") == 4
assert "ClusterRole" not in kinds and "ClusterRoleBinding" not in kinds
deployment = next(d for d in install if d["kind"] == "Deployment")
image = deployment["spec"]["template"]["spec"]["containers"][0]["image"]
assert image.startswith("registry.k8s.io/agent-sandbox/agent-sandbox-controller:v0.5.6@sha256:"), image
for doc in install:
    assert doc["metadata"]["annotations"]["blazn.dev/phase5-transaction"] == tx, doc["kind"]

rbac = [d for d in yaml.safe_load_all(open(f"{render}/production-rbac.yaml")) if d]
by = {(d["kind"], d["metadata"]["name"]): d for d in rbac}
assert set(by) == {
    ("ClusterRole", "blazn-agent-sandbox-observer"),
    ("ClusterRoleBinding", "blazn-agent-sandbox-observer"),
    ("Role", "blazn-agent-sandbox-system"),
    ("RoleBinding", "blazn-agent-sandbox-system"),
}
observer = by[("ClusterRole", "blazn-agent-sandbox-observer")]
for rule in observer["rules"]:
    assert set(rule["verbs"]) <= {"get", "list", "watch"}, "observer must stay read-only"
for doc in rbac:
    assert doc["metadata"]["annotations"]["blazn.dev/phase5-transaction"] == tx

bootstrap = [d for d in yaml.safe_load_all(open(f"{render}/bootstrap.yaml")) if d]
secret = next(d for d in bootstrap if d["kind"] == "Secret")
cert_pem = base64.b64decode(secret["data"]["tls.crt"])
enddate = subprocess.run(["openssl", "x509", "-noout", "-enddate"], input=cert_pem, capture_output=True, check=True).stdout.decode()
import datetime
not_after = datetime.datetime.strptime(enddate.strip().split("=", 1)[1], "%b %d %H:%M:%S %Y %Z")
lifetime = not_after - datetime.datetime.utcnow()
assert 390 <= lifetime.days <= 397, f"webhook certificate lifetime {lifetime.days}d is not production-lived"
job = next(d for d in bootstrap if d["kind"] == "Job")
assert len(job["spec"]["template"]["spec"]["initContainers"]) == 4
for doc in bootstrap:
    assert doc["metadata"]["annotations"]["blazn.dev/phase5-transaction"] == tx
print("phase5 installation render verified")
PY

printf 'Phase 5 installation static audit passed\n'

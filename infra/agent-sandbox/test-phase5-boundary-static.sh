#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
BOUNDARY=$ROOT/phase5-boundary
tmp=$(mktemp -d "${TMPDIR:-/tmp}/blazn-phase5-boundary-static.XXXXXX")
cleanup() {
  find "$tmp" -xdev -type f -delete
  find "$tmp" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM

for script in "$BOUNDARY"/*.sh; do sh -n "$script"; done
python3 -c 'import yaml' 2>/dev/null || { printf 'python3 yaml module is required\n' >&2; exit 1; }

if BLAZN_PHASE5_TRANSACTION_ID=not-a-uuid BLAZN_EXISTING_CLUSTER_QUEUE=m1-light "$BOUNDARY/render-boundary.sh" "$tmp/reject.yaml" 2>/dev/null; then exit 1; fi
if BLAZN_PHASE5_TRANSACTION_ID=99999999-9999-4999-8999-999999999999 BLAZN_EXISTING_CLUSTER_QUEUE='Bad Queue' "$BOUNDARY/render-boundary.sh" "$tmp/reject.yaml" 2>/dev/null; then exit 1; fi
BLAZN_PHASE5_TRANSACTION_ID=99999999-9999-4999-8999-999999999999 BLAZN_EXISTING_CLUSTER_QUEUE=m1-light "$BOUNDARY/render-boundary.sh" "$tmp/boundary.yaml" >/dev/null
[ "$(stat -c '%a' "$tmp/boundary.yaml" 2>/dev/null || stat -f '%Lp' "$tmp/boundary.yaml")" = 400 ]

python3 - "$tmp/boundary.yaml" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
by = {(d["kind"], d["metadata"]["name"]): d for d in docs}
assert len(by) == len(docs) == 8, f"expected 8 unique documents, got {len(docs)}"
tx = "99999999-9999-4999-8999-999999999999"
for d in docs:
    assert d["metadata"]["annotations"]["blazn.dev/phase5-transaction"] == tx, d["kind"]
for ns in ("blazn-poc-system", "blazn-poc-sandboxes"):
    doc = by[("Namespace", ns)]
    labels = doc["metadata"]["labels"]
    assert labels["pod-security.kubernetes.io/enforce"] == "restricted", ns
sa = by[("ServiceAccount", "blazn-sandbox-runner")]
assert sa["metadata"]["namespace"] == "blazn-poc-sandboxes"
assert sa["automountServiceAccountToken"] is False
lq = by[("LocalQueue", "blazn-poc")]
assert lq["apiVersion"] == "kueue.x-k8s.io/v1beta1"
assert lq["metadata"]["namespace"] == "blazn-poc-sandboxes"
assert lq["spec"]["clusterQueue"] == "m1-light"
role = by[("Role", "blazn-agent-sandbox-controller")]
assert role["metadata"]["namespace"] == "blazn-poc-sandboxes"
assert all("clusterrole" not in d["kind"].lower() for d in docs), "no cluster-wide RBAC"
binding = by[("RoleBinding", "blazn-agent-sandbox-controller")]
subject = binding["subjects"][0]
assert (subject["name"], subject["namespace"]) == ("agent-sandbox-controller", "agent-sandbox-system")
policy = by[("ValidatingAdmissionPolicy", "blazn-sandbox-boundary")]
assert policy["spec"]["failurePolicy"] == "Fail"
selector = policy["spec"]["matchConstraints"]["namespaceSelector"]["matchExpressions"][0]
assert selector == {"key": "kubernetes.io/metadata.name", "operator": "In", "values": ["blazn-poc-sandboxes"]}
rules = policy["spec"]["matchConstraints"]["resourceRules"][0]
assert rules["apiVersions"] == ["v1alpha1", "v1beta1"]
assert rules["resources"] == ["sandboxes", "sandboxes/status", "sandboxes/finalizers"]
expressions = "\n".join(v["expression"] for v in policy["spec"]["validations"])
for needle in (
    "blazn-poc-sandboxes",
    "^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$",
    "blazn.dev/managed",
    "kueue.x-k8s.io/queue-name",
    "blazn-sandbox-runner",
    "automountServiceAccountToken == false",
    "blazn.dev/sandbox-eligible",
    "hostNetwork",
    "runAsUser == 65532",
    "@sha256:[0-9a-f]{64}$",
    "quantity(",
    "system:serviceaccount:blazn-poc-system:blazn-sandbox-controller",
    "system:serviceaccount:agent-sandbox-system:agent-sandbox-controller",
    "object.spec == oldObject.spec",
    "annotations.filter(k, k != 'agents.x-k8s.io/pod-name')",
    "sandboxes.blazn.dev/trust-level",
):
    assert needle in expressions, f"policy lost required rule: {needle}"
policy_binding = by[("ValidatingAdmissionPolicyBinding", "blazn-sandbox-boundary")]
assert policy_binding["spec"]["validationActions"] == ["Deny"]
assert policy_binding["spec"]["policyName"] == "blazn-sandbox-boundary"
print("boundary render structure verified")
PY

python3 "$BOUNDARY/good-sandbox.py" >"$tmp/good.json"
python3 - "$tmp/good.json" <<'PY'
import json, sys
doc = json.load(open(sys.argv[1]))
pod = doc["spec"]["podTemplate"]["spec"]
assert doc["metadata"]["namespace"] == "blazn-poc-sandboxes"
assert pod["serviceAccountName"] == "blazn-sandbox-runner"
assert pod["securityContext"]["runAsUser"] == 65532
assert len(pod["containers"]) == 1
assert "@sha256:" in pod["containers"][0]["image"]
print("good sandbox fixture verified")
PY

printf 'Phase 5 boundary static audit passed\n'

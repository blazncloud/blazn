#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/versions.env"
# shellcheck disable=SC1091
. "$ROOT/lib.sh"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/blazn-as-static.XXXXXX")
cleanup() {
  find "$tmp" -xdev -type f -delete
  find "$tmp" -xdev -depth -type d -empty -delete
}
abort() {
  trap - EXIT HUP INT TERM
  cleanup
  exit 130
}
trap cleanup EXIT
trap abort HUP INT TERM

curl -fsSL "$AGENT_SANDBOX_MANIFEST_URL" -o "$tmp/agent-sandbox.yaml"
printf '%s  %s\n' "$AGENT_SANDBOX_MANIFEST_SHA256" "$tmp/agent-sandbox.yaml" | sha256sum -c - >/dev/null
curl -fsSL "$KUEUE_MANIFEST_URL" -o "$tmp/kueue.yaml"
printf '%s  %s\n' "$KUEUE_MANIFEST_SHA256" "$tmp/kueue.yaml" | sha256sum -c - >/dev/null
pin_controller_images "$tmp/agent-sandbox.yaml" "$tmp/kueue.yaml"

[ "$(grep -c '^kind: CustomResourceDefinition$' "$tmp/agent-sandbox.yaml")" -eq 4 ]
[ "$(grep -c '^kind: ClusterRole$' "$tmp/agent-sandbox.yaml")" -eq 2 ]
[ "$(grep -c '^kind: ClusterRoleBinding$' "$tmp/agent-sandbox.yaml")" -eq 2 ]
[ "$(grep -c '^kind: CustomResourceDefinition$' "$tmp/kueue.yaml")" -eq 11 ]
grep -F "image: $AGENT_SANDBOX_IMAGE" "$tmp/agent-sandbox.yaml" >/dev/null
grep -F "image: $KUEUE_IMAGE" "$tmp/kueue.yaml" >/dev/null
grep -F 'sandboxes.agents.x-k8s.io' "$tmp/agent-sandbox.yaml" >/dev/null
grep -F 'sandboxclaims.extensions.agents.x-k8s.io' "$tmp/agent-sandbox.yaml" >/dev/null
grep -F 'sandboxwarmpools.extensions.agents.x-k8s.io' "$tmp/agent-sandbox.yaml" >/dev/null
grep -F 'sandboxtemplates.extensions.agents.x-k8s.io' "$tmp/agent-sandbox.yaml" >/dev/null

# This intentionally locates the literal shell variable in the lifecycle test.
# shellcheck disable=SC2016
delete_line=$(grep -n 'delete cluster --name "$cluster";' "$ROOT/test-disposable.sh" | tail -1 | cut -d: -f1)
residue_line=$(grep -n 'network ls --format' "$ROOT/test-disposable.sh" | cut -d: -f1)
disarm_line=$(grep -n '^creation_attempted=0$' "$ROOT/test-disposable.sh" | tail -1 | cut -d: -f1)
[ "$delete_line" -lt "$residue_line" ]
[ "$residue_line" -lt "$disarm_line" ]

trap - EXIT HUP INT TERM
cleanup
printf 'Agent Sandbox and Kueue pinned-manifest audit passed\n'

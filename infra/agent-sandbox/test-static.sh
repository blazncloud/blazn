#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$ROOT/versions.env"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/blazn-as-static.XXXXXX")
cleanup() {
  find "$tmp" -xdev -type f -delete
  find "$tmp" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM

curl -fsSL "$AGENT_SANDBOX_MANIFEST_URL" -o "$tmp/agent-sandbox.yaml"
printf '%s  %s\n' "$AGENT_SANDBOX_MANIFEST_SHA256" "$tmp/agent-sandbox.yaml" | sha256sum -c - >/dev/null
curl -fsSL "$KUEUE_MANIFEST_URL" -o "$tmp/kueue.yaml"
printf '%s  %s\n' "$KUEUE_MANIFEST_SHA256" "$tmp/kueue.yaml" | sha256sum -c - >/dev/null

[ "$(grep -c '^kind: CustomResourceDefinition$' "$tmp/agent-sandbox.yaml")" -eq 4 ]
[ "$(grep -c '^kind: ClusterRole$' "$tmp/agent-sandbox.yaml")" -eq 2 ]
[ "$(grep -c '^kind: ClusterRoleBinding$' "$tmp/agent-sandbox.yaml")" -eq 2 ]
[ "$(grep -c '^kind: CustomResourceDefinition$' "$tmp/kueue.yaml")" -eq 11 ]
grep -F 'image: registry.k8s.io/agent-sandbox/agent-sandbox-controller:v0.5.6' "$tmp/agent-sandbox.yaml" >/dev/null
grep -F 'image: registry.k8s.io/kueue/kueue:v0.19.2' "$tmp/kueue.yaml" >/dev/null
grep -F 'sandboxes.agents.x-k8s.io' "$tmp/agent-sandbox.yaml" >/dev/null
grep -F 'sandboxclaims.extensions.agents.x-k8s.io' "$tmp/agent-sandbox.yaml" >/dev/null
grep -F 'sandboxwarmpools.extensions.agents.x-k8s.io' "$tmp/agent-sandbox.yaml" >/dev/null
grep -F 'sandboxtemplates.extensions.agents.x-k8s.io' "$tmp/agent-sandbox.yaml" >/dev/null

trap - EXIT HUP INT TERM
cleanup
printf 'Agent Sandbox and Kueue pinned-manifest audit passed\n'

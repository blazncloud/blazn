#!/bin/sh
set -eu

[ "$#" -eq 5 ] || { printf 'usage: %s BLAZN_BIN WORKSPACE TEMPLATE_REFERENCE SOURCE_COMMIT PATCH_OUTPUT\n' "$0" >&2; exit 64; }
blazn=$1
workspace=$2
template_reference=$3
source_commit=$4
patch_output=$5
root=$(CDPATH='' cd -- "$(dirname -- "$0")/../../.." && pwd)
[ -x "$blazn" ] || { printf 'Blazn CLI is not executable\n' >&2; exit 1; }
case $workspace in ????????-????-????-????-????????????) ;; *) printf 'workspace must be a UUID\n' >&2; exit 1 ;; esac
case $source_commit in *[!0-9a-f]*|'') printf 'source commit must be lowercase hexadecimal\n' >&2; exit 1 ;; esac
case ${#source_commit} in 40|64) ;; *) printf 'source commit must contain 40 or 64 hexadecimal characters\n' >&2; exit 1 ;; esac
[ "$(git -C "$root" rev-parse HEAD)" = "$source_commit" ] || { printf 'source commit must equal the test checkout HEAD\n' >&2; exit 1; }
[ ! -e "$patch_output" ] || { printf 'patch output already exists\n' >&2; exit 1; }
for command in jq git mktemp cmp; do command -v "$command" >/dev/null 2>&1 || { printf '%s is required\n' "$command" >&2; exit 1; }; done

work=$(mktemp -d "${TMPDIR:-/tmp}/blazn-coding-agent-live.XXXXXX")
sandbox_id=
delete_requested=0
request_prefix=coding-agent-live-$(date -u '+%Y%m%d%H%M%S')-$$
cleanup() {
  if [ -n "$sandbox_id" ] && [ "$delete_requested" -eq 0 ]; then
    "$blazn" --output json sandbox delete "$sandbox_id" --request-id "$request_prefix-cleanup" >/dev/null 2>&1 ||
      printf 'warning: automatic Sandbox cleanup failed for %s\n' "$sandbox_id" >&2
  fi
  find "$work" -xdev -type f -delete
  find "$work" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM

printf 'Blazn coding-agent E2E: creating Sandbox\n'
"$blazn" --output json sandbox create --template "$template_reference" --arch amd64 --mode direct --expires 15m \
  --approved-non-sensitive --workspace "$workspace" --source "source=$source_commit" --request-id "$request_prefix-create" >"$work/create.json"
sandbox_id=$(jq -er '.sandbox.id' "$work/create.json")

attempt=0
while [ "$attempt" -lt 120 ]; do
  attempt=$((attempt + 1))
  "$blazn" --output json sandbox get "$sandbox_id" >"$work/state.json"
  state=$(jq -er '.state' "$work/state.json")
  [ "$state" = ready ] && break
  case $state in failed|recovery_required|deleted) printf 'Sandbox entered terminal create state %s\n' "$state" >&2; exit 1 ;; esac
  sleep 5
done
[ "${state:-}" = ready ] || { printf 'Sandbox did not become ready\n' >&2; exit 1; }

printf 'Blazn coding-agent E2E: executing immutable task\n'
"$blazn" --output json sandbox exec "$sandbox_id" -- node /workspace/src/blazn/examples/coding-agent/src/coding-agent.mjs \
  --task /workspace/src/blazn/examples/coding-agent/fixtures/task.json --source-root /workspace/src/blazn \
  --output /workspace/artifacts/change.patch >"$work/exec.json"
jq -e '.remoteExitCode == 0 and .truncated == false' "$work/exec.json" >/dev/null

"$blazn" --output json sandbox download "$sandbox_id" /workspace/artifacts/change.patch "$work/change.patch" >"$work/download.json"
printf '%s\n' \
  '--- a/examples/coding-agent/fixtures/source/calculator.mjs' \
  '+++ b/examples/coding-agent/fixtures/source/calculator.mjs' \
  '@@ -1,3 +1,3 @@' \
  ' export function add(left, right) {' \
  '-  return left - right;' \
  '+  return left + right;' \
  ' }' >"$work/expected.patch"
cmp "$work/expected.patch" "$work/change.patch"
git -C "$root" apply --check "$work/change.patch"
cp "$work/change.patch" "$patch_output"

printf 'Blazn coding-agent E2E: deleting Sandbox and exporting required Artifact\n'
"$blazn" --output json sandbox delete "$sandbox_id" --request-id "$request_prefix-delete" >"$work/delete.json"
delete_requested=1
attempt=0
while [ "$attempt" -lt 120 ]; do
  attempt=$((attempt + 1))
  "$blazn" --output json sandbox get "$sandbox_id" >"$work/deleted.json"
  state=$(jq -er '.state' "$work/deleted.json")
  [ "$state" = deleted ] && break
  case $state in failed|recovery_required) printf 'Sandbox entered terminal cleanup state %s\n' "$state" >&2; exit 1 ;; esac
  sleep 2
done
[ "${state:-}" = deleted ] || { printf 'Sandbox did not reach deleted state\n' >&2; exit 1; }

printf 'Blazn coding-agent acceptance passed for Sandbox %s; patch: %s\n' "$sandbox_id" "$patch_output"

#!/bin/sh
set -eu

if [ "$#" -lt 5 ] || [ "$#" -gt 6 ]; then
  printf 'usage: %s BLAZN_BIN WORKSPACE TEMPLATE_FILE TEMPLATE_REFERENCE SOURCE_COMMIT [amd64|arm64]\n' "$0" >&2
  exit 64
fi

blazn=$1
workspace=$2
template_file=$3
template_reference=$4
source_commit=$5
architecture=${6:-amd64}

[ -x "$blazn" ] || { printf 'Blazn CLI is not executable: %s\n' "$blazn" >&2; exit 1; }
[ -f "$template_file" ] || { printf 'template file is missing: %s\n' "$template_file" >&2; exit 1; }
case $workspace in ????????-????-????-????-????????????) ;; *) printf 'workspace must be a UUID\n' >&2; exit 1 ;; esac
case $source_commit in
  *[!0-9a-f]*|'') printf 'source commit must be lowercase hexadecimal\n' >&2; exit 1 ;;
esac
case ${#source_commit} in 40|64) ;; *) printf 'source commit must contain 40 or 64 hexadecimal characters\n' >&2; exit 1 ;; esac
case $architecture in amd64|arm64) ;; *) printf 'architecture must be amd64 or arm64\n' >&2; exit 1 ;; esac
for command in jq cmp date mktemp base64; do command -v "$command" >/dev/null 2>&1 || { printf '%s is required\n' "$command" >&2; exit 1; }; done
if printf 'Zg==\n' | base64 --decode >/dev/null 2>&1; then
  base64_mode=long
elif printf 'Zg==\n' | base64 -D >/dev/null 2>&1; then
  base64_mode=darwin
else
  printf 'base64 decoder is unavailable\n' >&2
  exit 1
fi

decode_base64() {
  if [ "$base64_mode" = long ]; then base64 --decode; else base64 -D; fi
}

run_remote_job() {
  job_name=$1
  local_script=$2
  remote_root=/workspace/artifacts/e2e-$job_name
  "$blazn" --output json sandbox upload "$sandbox_id" "$local_script" "$remote_root.sh" >"$work/$job_name-upload.json"
  "$blazn" --output json sandbox exec "$sandbox_id" -- sh -c \
    "rm -f '$remote_root.status' '$remote_root.status.tmp'; nohup sh '$remote_root.sh' >'$remote_root.log' 2>&1 </dev/null &" >"$work/$job_name-launch.json"
  jq -e '.remoteExitCode == 0 and .truncated == false' "$work/$job_name-launch.json" >/dev/null
  job_attempt=0
  while [ "$job_attempt" -lt 120 ]; do
    job_attempt=$((job_attempt + 1))
    "$blazn" --output json sandbox exec "$sandbox_id" -- sh -c \
      "if test -f '$remote_root.status'; then cat '$remote_root.status'; else printf running; fi" >"$work/$job_name-status.json"
    jq -er '.stdoutBase64' "$work/$job_name-status.json" | decode_base64 >"$work/$job_name-status.txt"
    job_status=$(tr -d '\r\n' <"$work/$job_name-status.txt")
    case $job_status in
      running) sleep 5 ;;
      0) break ;;
      *)
        "$blazn" --output json sandbox download "$sandbox_id" "$remote_root.log" "$work/$job_name.log" >/dev/null || true
        tail -40 "$work/$job_name.log" >&2 || true
        printf '%s job failed with status %s\n' "$job_name" "$job_status" >&2
        exit 1
        ;;
    esac
  done
  [ "${job_status:-}" = 0 ] || { printf '%s job did not finish within ten minutes\n' "$job_name" >&2; exit 1; }
  "$blazn" --output json sandbox download "$sandbox_id" "$remote_root.log" "$work/$job_name.log" >"$work/$job_name-download.json"
}

work=$(mktemp -d "${TMPDIR:-/tmp}/blazn-development-live.XXXXXX")
sandbox_id=
delete_requested=0
request_prefix=development-e2e-$(date -u '+%Y%m%d%H%M%S')-$$

cleanup() {
  if [ -n "$sandbox_id" ] && [ "$delete_requested" -eq 0 ]; then
    "$blazn" --output json sandbox delete "$sandbox_id" --request-id "$request_prefix-cleanup" >/dev/null 2>&1 || \
      printf 'warning: automatic Sandbox cleanup failed for %s\n' "$sandbox_id" >&2
  fi
  case $work in
    "${TMPDIR:-/tmp}"/blazn-development-live.*) rm -r -- "$work" ;;
    *) printf 'refusing to remove unexpected test directory: %s\n' "$work" >&2 ;;
  esac
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

"$blazn" --output json template validate -f "$template_file" | jq -e '.valid == true' >"$work/template-validation.json"
if [ "${BLAZN_SKIP_TEMPLATE_PUBLISH:-0}" != 1 ]; then
  "$blazn" --output json template publish -f "$template_file" --workspace "$workspace" --request-id "$request_prefix-publish" >"$work/template-publish.json"
  jq -e '.template.id and .version.id' "$work/template-publish.json" >/dev/null
fi

"$blazn" --output json sandbox create \
  --template "$template_reference" \
  --arch "$architecture" \
  --mode direct \
  --expires 2h \
  --approved-non-sensitive \
  --workspace "$workspace" \
  --source "source=$source_commit" \
  --request-id "$request_prefix-create" >"$work/create.json"
sandbox_id=$(jq -er '.sandbox.id' "$work/create.json")

ready_attempt=0
while [ "$ready_attempt" -lt 120 ]; do
  ready_attempt=$((ready_attempt + 1))
  "$blazn" --output json sandbox get "$sandbox_id" >"$work/ready.json"
  state=$(jq -er '.state' "$work/ready.json")
  [ "$state" = ready ] && break
  case $state in failed|recovery_required|deleted) printf 'Sandbox entered terminal create state: %s\n' "$state" >&2; exit 1 ;; esac
  sleep 5
done
[ "${state:-}" = ready ] || { printf 'Sandbox did not reach ready state within ten minutes\n' >&2; exit 1; }
jq -e '.desiredState == "ready"' "$work/ready.json" >/dev/null
# Replay the complete event stream after readiness so even clients with a
# short per-request deadline exercise and verify the terminal watch contract.
"$blazn" sandbox watch "$sandbox_id" >"$work/watch.jsonl"
jq -e 'select(.type == "sandbox.ready")' "$work/watch.jsonl" >/dev/null

"$blazn" --output json sandbox exec "$sandbox_id" -- sh -lc \
  'go version && node --version && npm --version && git --version && test -w /workspace/src/blazn && test -w /workspace/artifacts' >"$work/toolchains.json"
jq -e '.remoteExitCode == 0 and .truncated == false' "$work/toolchains.json" >/dev/null

# The generated job scripts expand $code inside the Sandbox, not here.
# shellcheck disable=SC2016
printf '%s\n' '#!/bin/sh' 'set +e' \
  'cd /workspace/src/blazn && USER=blazn LOGNAME=blazn go test $(go list ./... | grep -Ev "^github.com/blazncloud/blazn/internal/(node|workspace)$")' \
  'code=$?' \
  'printf "%s\n" "$code" > /workspace/artifacts/e2e-go.status.tmp' \
  'mv /workspace/artifacts/e2e-go.status.tmp /workspace/artifacts/e2e-go.status' \
  'exit "$code"' >"$work/go-job.sh"
run_remote_job go "$work/go-job.sh"

# shellcheck disable=SC2016
printf '%s\n' '#!/bin/sh' 'set +e' \
  'cd /workspace/src/blazn/services/control-api && npm run build && npm test && cd /workspace/src/blazn/examples/coding-agent && npm test' \
  'code=$?' \
  'printf "%s\n" "$code" > /workspace/artifacts/e2e-node.status.tmp' \
  'mv /workspace/artifacts/e2e-node.status.tmp /workspace/artifacts/e2e-node.status' \
  'exit "$code"' >"$work/node-job.sh"
run_remote_job node "$work/node-job.sh"

printf 'blazn development upload/download acceptance\n' >"$work/upload.txt"
"$blazn" --output json sandbox upload "$sandbox_id" "$work/upload.txt" /workspace/artifacts/e2e-upload.txt >"$work/upload.json"
"$blazn" --output json sandbox download "$sandbox_id" /workspace/artifacts/e2e-upload.txt "$work/download.txt" >"$work/download.json"
cmp "$work/upload.txt" "$work/download.txt"

"$blazn" --output json sandbox exec "$sandbox_id" -- sh -lc \
  'cd /workspace/src/blazn && printf "\n<!-- blazn-development-e2e -->\n" >> README.md && git diff --binary -- README.md > /workspace/artifacts/change.patch && test -s /workspace/artifacts/change.patch' >"$work/artifact.json"
jq -e '.remoteExitCode == 0 and .truncated == false' "$work/artifact.json" >/dev/null

"$blazn" --output json sandbox delete "$sandbox_id" --request-id "$request_prefix-delete" >"$work/delete.json"
delete_requested=1

attempt=0
while [ "$attempt" -lt 120 ]; do
  attempt=$((attempt + 1))
  "$blazn" --output json sandbox get "$sandbox_id" >"$work/deleted.json"
  state=$(jq -er '.state' "$work/deleted.json")
  [ "$state" = deleted ] && break
  case $state in failed|recovery_required) printf 'Sandbox entered terminal cleanup state: %s\n' "$state" >&2; exit 1 ;; esac
  sleep 2
done
[ "${state:-}" = deleted ] || { printf 'Sandbox did not reach deleted state\n' >&2; exit 1; }
jq -e '.desiredState == "deleted"' "$work/deleted.json" >/dev/null

printf 'Blazn CLI development acceptance passed for Sandbox %s (%s).\n' "$sandbox_id" "$architecture"

#!/bin/sh
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
read_retry=$script_dir/read-retry.sh
delete_and_prove=$script_dir/delete-and-prove.sh
download_patch=$script_dir/download-patch.sh
repo_root=$(CDPATH='' cd -- "$script_dir/../.." && pwd)

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
for command in jq cmp date mktemp base64 git; do command -v "$command" >/dev/null 2>&1 || { printf '%s is required\n' "$command" >&2; exit 1; }; done
if [ "${BLAZN_SOURCE_PREFLIGHT_FETCH:-1}" = 1 ]; then
  git -C "$repo_root" fetch --quiet --no-tags origin || { printf '%s\n' 'unable to refresh origin before source preflight' >&2; exit 1; }
fi
git -C "$repo_root" cat-file -e "$source_commit^{commit}" 2>/dev/null || { printf 'source commit is not present locally: %s\n' "$source_commit" >&2; exit 1; }
origin_ref=$(git -C "$repo_root" for-each-ref --format='%(refname)' --contains "$source_commit" refs/remotes/origin/ | head -1)
[ -n "$origin_ref" ] || { printf 'source commit is not reachable from a known origin ref: %s\n' "$source_commit" >&2; exit 1; }
patch_default_dir=${BLAZN_DEVELOPMENT_PATCH_DEFAULT_DIR:-}
patch_output=${BLAZN_DEVELOPMENT_PATCH_OUTPUT:-}
if [ -z "$patch_output" ]; then
  patch_default_dir=$(mktemp -d "${TMPDIR:-/tmp}/blazn-development-output.XXXXXX")
  patch_output=$patch_default_dir/change-${source_commit}.patch
fi
patch_parent=$(dirname -- "$patch_output")
[ -d "$patch_parent" ] && [ -w "$patch_parent" ] || { printf 'patch output directory is not writable: %s\n' "$patch_parent" >&2; exit 1; }
patch_output=$(CDPATH='' cd -- "$patch_parent" && pwd)/$(basename -- "$patch_output")
patch_checksum=$patch_output.sha256
[ ! -e "$patch_output" ] && [ ! -e "$patch_checksum" ] || { printf 'refusing to overwrite patch output or checksum: %s\n' "$patch_output" >&2; exit 1; }
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

stage() { printf 'Blazn development E2E: %s\n' "$1"; }

run_remote_job() {
  job_name=$1
  local_script=$2
  stage "running $job_name test job"
  remote_root=/workspace/artifacts/e2e-$job_name
  "$blazn" --output json sandbox upload "$sandbox_id" "$local_script" "$remote_root.sh" >"$work/$job_name-upload.json"
  "$blazn" --output json sandbox exec "$sandbox_id" -- sh -c \
    "rm -f '$remote_root.status' '$remote_root.status.tmp'; nohup sh '$remote_root.sh' >'$remote_root.log' 2>&1 </dev/null &" >"$work/$job_name-launch.json"
  jq -e '.remoteExitCode == 0 and .truncated == false' "$work/$job_name-launch.json" >/dev/null
  job_attempt=0
  while [ "$job_attempt" -lt 120 ]; do
    job_attempt=$((job_attempt + 1))
    "$read_retry" "$work/$job_name-status.json" "$blazn" --output json sandbox exec "$sandbox_id" -- sh -c \
      "if test -f '$remote_root.status'; then cat '$remote_root.status'; else printf running; fi"
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
cleanup_proven=0
create_started=0
request_prefix=development-e2e-$(date -u '+%Y%m%d%H%M%S')-$$

cleanup() {
  original_status=$?
  trap - EXIT
  cleanup_failed=0
  if [ -n "$sandbox_id" ] && [ "$cleanup_proven" -eq 0 ]; then
    printf 'Blazn development E2E: proving cleanup for Sandbox %s\n' "$sandbox_id" >&2
    if ! sh "$delete_and_prove" "$read_retry" "$blazn" "$sandbox_id" "$request_prefix-delete" "$work"; then
      printf 'ERROR: Sandbox %s deletion is unproven; evidence retained at %s\n' "$sandbox_id" "$work" >&2
      cleanup_failed=1
    fi
  elif [ "$create_started" -eq 1 ] && [ -z "$sandbox_id" ]; then
    printf 'ERROR: create outcome is ambiguous and no exact Sandbox ID was recovered; evidence retained at %s\n' "$work" >&2
    cleanup_failed=1
  fi
  if [ "${BLAZN_E2E_KEEP_EVIDENCE:-0}" = 1 ] || [ "$cleanup_failed" -eq 1 ]; then
    printf 'Blazn development E2E evidence retained at %s\n' "$work"
  else
    case $work in
      "${TMPDIR:-/tmp}"/blazn-development-live.*) rm -r -- "$work" ;;
      *) printf 'refusing to remove unexpected test directory: %s\n' "$work" >&2 ;;
    esac
  fi
  if [ -n "$patch_default_dir" ]; then rmdir -- "$patch_default_dir" 2>/dev/null || true; fi
  if [ "$cleanup_failed" -eq 1 ] && [ "$original_status" -eq 0 ]; then original_status=1; fi
  exit "$original_status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

stage 'validating development template'
"$blazn" --output json template validate -f "$template_file" | jq -e '.valid == true' >"$work/template-validation.json"
if [ "${BLAZN_SKIP_TEMPLATE_PUBLISH:-0}" != 1 ]; then
  stage 'publishing development template'
  "$blazn" --output json template publish -f "$template_file" --workspace "$workspace" --request-id "$request_prefix-publish" >"$work/template-publish.json"
  jq -e '.template.id and .version.id' "$work/template-publish.json" >/dev/null
fi

stage 'creating development Sandbox'
create_started=1
"$read_retry" "$work/create.json" "$blazn" --output json sandbox create \
  --template "$template_reference" \
  --arch "$architecture" \
  --mode direct \
  --expires 2h \
  --approved-non-sensitive \
  --workspace "$workspace" \
  --source "source=$source_commit" \
  --request-id "$request_prefix-create"
sandbox_id=$(jq -er '.sandbox.id' "$work/create.json")

ready_attempt=0
stage 'waiting for Sandbox readiness and replaying watch'
while [ "$ready_attempt" -lt 120 ]; do
  ready_attempt=$((ready_attempt + 1))
  "$read_retry" "$work/ready.json" "$blazn" --output json sandbox get "$sandbox_id"
  state=$(jq -er '.state' "$work/ready.json")
  [ "$state" = ready ] && break
  case $state in failed|recovery_required|deleted) printf 'Sandbox entered terminal create state: %s\n' "$state" >&2; exit 1 ;; esac
  sleep 5
done
[ "${state:-}" = ready ] || { printf 'Sandbox did not reach ready state within ten minutes\n' >&2; exit 1; }
jq -e '.desiredState == "ready"' "$work/ready.json" >/dev/null
# Replay the complete event stream after readiness so even clients with a
# short per-request deadline exercise and verify the terminal watch contract.
"$read_retry" "$work/watch.jsonl" "$blazn" sandbox watch "$sandbox_id"
jq -e 'select(.type == "sandbox.ready")' "$work/watch.jsonl" >/dev/null

stage 'verifying development toolchains'
"$blazn" --output json sandbox exec "$sandbox_id" -- sh -lc \
  'go version && node --version && npm --version && git --version && test -w /workspace/src/blazn && test -w /workspace/artifacts' >"$work/toolchains.json"
jq -e '.remoteExitCode == 0 and .truncated == false' "$work/toolchains.json" >/dev/null

# The generated job scripts expand $code inside the Sandbox, not here.
# shellcheck disable=SC2016
printf '%s\n' '#!/bin/sh' 'set +e' \
  'cd /workspace/src/blazn && USER=blazn LOGNAME=blazn go test -timeout 2m $(go list ./... | grep -Ev "^github.com/blazncloud/blazn/internal/(node|workspace)$")' \
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
stage 'verifying upload and download'
"$blazn" --output json sandbox upload "$sandbox_id" "$work/upload.txt" /workspace/artifacts/e2e-upload.txt >"$work/upload.json"
"$blazn" --output json sandbox download "$sandbox_id" /workspace/artifacts/e2e-upload.txt "$work/download.txt" >"$work/download.json"
cmp "$work/upload.txt" "$work/download.txt"

stage 'generating patch artifact from the materialized source snapshot'
# The command expands $code inside the Sandbox, not in this runner.
# shellcheck disable=SC2016
"$blazn" --output json sandbox exec "$sandbox_id" -- sh -lc \
  'cd /workspace/src/blazn || exit; cp README.md README.md.before || exit; printf "\n<!-- blazn-development-e2e -->\n" >> README.md || exit; set +e; git diff --no-index --binary -- README.md.before README.md > /workspace/artifacts/change.patch; code=$?; set -e; rm -f README.md.before; test "$code" -eq 1; sed -i "s|a/README.md.before|a/README.md|g" /workspace/artifacts/change.patch; test -s /workspace/artifacts/change.patch' >"$work/artifact.json"
jq -e '.remoteExitCode == 0 and .truncated == false' "$work/artifact.json" >/dev/null

stage "downloading durable patch artifact to $patch_output"
actual_patch_sha=$(sh "$download_patch" "$blazn" "$sandbox_id" "$patch_output" "$work/patch-download.json")
printf 'Blazn development E2E: patch verified at %s (%s)\n' "$patch_output" "$actual_patch_sha"

stage 'stopping Sandbox and proving terminal state'
"$blazn" --output json sandbox stop "$sandbox_id" --request-id "$request_prefix-stop" >"$work/stop.json"
stop_attempt=0
while [ "$stop_attempt" -lt 120 ]; do
  stop_attempt=$((stop_attempt + 1))
  "$read_retry" "$work/stopped.json" "$blazn" --output json sandbox get "$sandbox_id"
  state=$(jq -er '.state' "$work/stopped.json")
  [ "$state" = stopped ] && break
  case $state in failed|recovery_required|deleted) printf 'Sandbox entered unexpected stop state: %s\n' "$state" >&2; exit 1 ;; esac
  sleep 2
done
[ "${state:-}" = stopped ] || { printf 'Sandbox did not reach stopped state\n' >&2; exit 1; }
jq -e '.desiredState == "stopped"' "$work/stopped.json" >/dev/null

stage 'deleting Sandbox and proving terminal state'
sh "$delete_and_prove" "$read_retry" "$blazn" "$sandbox_id" "$request_prefix-delete" "$work"
cleanup_proven=1

printf 'Blazn CLI development acceptance passed for Sandbox %s (%s).\n' "$sandbox_id" "$architecture"

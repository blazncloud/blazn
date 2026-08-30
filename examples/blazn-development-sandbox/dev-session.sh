#!/bin/sh
set -eu

usage() {
  cat <<'EOF'
usage: dev-session.sh [--receipt PATH] [--blazn PATH] COMMAND [ARGUMENTS]

Commands:
  start --workspace UUID [--source COMMIT] [--arch amd64|arm64] [--expires DURATION]
  status
  exec -- COMMAND [ARGUMENT ...]
  upload LOCAL_PATH REMOTE_PATH
  download REMOTE_PATH LOCAL_PATH
  patch OUTPUT_PATH
  finish --patch OUTPUT_PATH | --discard

The default receipt is $XDG_STATE_HOME/blazn/development-session.json, or
$HOME/.local/state/blazn/development-session.json when XDG_STATE_HOME is unset.
It contains identifiers and lifecycle state only; credentials are never stored.
EOF
}

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/../.." && pwd)
read_retry=$script_dir/read-retry.sh
delete_and_prove=$script_dir/delete-and-prove.sh
blazn=${BLAZN_BIN:-}
receipt=${BLAZN_DEVELOPMENT_RECEIPT:-${XDG_STATE_HOME:-${HOME:?HOME is required}/.local/state}/blazn/development-session.json}

while [ "$#" -gt 0 ]; do
  case $1 in
    --receipt) [ "$#" -ge 2 ] || { printf '%s\n' '--receipt requires a value' >&2; exit 64; }; receipt=$2; shift 2 ;;
    --blazn) [ "$#" -ge 2 ] || { printf '%s\n' '--blazn requires a value' >&2; exit 64; }; blazn=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    --*) printf 'unknown global option: %s\n' "$1" >&2; usage >&2; exit 64 ;;
    *) break ;;
  esac
done
[ "$#" -gt 0 ] || { usage >&2; exit 64; }
action=$1
shift

for dependency in jq mktemp date git; do
  command -v "$dependency" >/dev/null 2>&1 || { printf '%s is required\n' "$dependency" >&2; exit 1; }
done

decode_base64() {
  if printf 'Zg==\n' | base64 --decode >/dev/null 2>&1; then base64 --decode; else base64 -D; fi
}

duration_seconds() {
  duration=$1
  number=${duration%?}
  unit=${duration#"$number"}
  case $number in ''|*[!0-9]*|0) return 1 ;; esac
  case $unit in
    s) [ "${#number}" -le 4 ] && [ "$number" -ge 60 ] && [ "$number" -le 7200 ] || return 1; printf '%s\n' "$number" ;;
    m) [ "${#number}" -le 3 ] && [ "$number" -le 120 ] || return 1; printf '%s\n' "$((number * 60))" ;;
    h) [ "${#number}" -le 1 ] && [ "$number" -le 2 ] || return 1; printf '%s\n' "$((number * 3600))" ;;
    *) return 1 ;;
  esac
}

receipt_parent=$(dirname -- "$receipt")
mkdir -p -- "$receipt_parent"
receipt_parent=$(CDPATH='' cd -- "$receipt_parent" && pwd)
receipt=$receipt_parent/$(basename -- "$receipt")
evidence=$receipt.evidence
receipt_lock=$receipt.lock
if ! mkdir -- "$receipt_lock" 2>/dev/null; then
  printf 'development session receipt is busy: %s (remove %s only after proving no session command is running)\n' "$receipt" "$receipt_lock" >&2
  exit 1
fi
cleanup_file=
cleanup() {
  [ -z "$cleanup_file" ] || rm -f -- "$cleanup_file"
  rmdir -- "$receipt_lock" 2>/dev/null || true
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
umask 077
[ ! -L "$evidence" ] || { printf 'refusing symlink evidence directory: %s\n' "$evidence" >&2; exit 1; }
mkdir -p -- "$evidence"

write_receipt() {
  temporary=$(mktemp "$receipt_parent/.blazn-receipt.XXXXXX")
  cleanup_file=$temporary
  jq -n \
    --arg schema blazn-development-session/v1 \
    --arg phase "$phase" \
    --arg sandboxId "${sandbox_id:-}" \
    --arg workspaceId "$workspace" \
    --arg sourceCommit "$source_commit" \
    --arg architecture "$architecture" \
    --arg template "$template_reference" \
    --arg expires "$expires" \
    --arg requestPrefix "$request_prefix" \
    --arg createdAt "$created_at" \
    '{schema:$schema, phase:$phase, sandboxId:(if $sandboxId == "" then null else $sandboxId end), workspaceId:$workspaceId, sourceCommit:$sourceCommit, architecture:$architecture, template:$template, expires:$expires, requestPrefix:$requestPrefix, createdAt:$createdAt}' >"$temporary"
  chmod 600 "$temporary"
  mv -- "$temporary" "$receipt"
  cleanup_file=
}

load_receipt() {
  [ -f "$receipt" ] || { printf 'development session receipt does not exist: %s\n' "$receipt" >&2; exit 1; }
  jq -e '.schema == "blazn-development-session/v1"' "$receipt" >/dev/null || { printf 'invalid development session receipt: %s\n' "$receipt" >&2; exit 1; }
  phase=$(jq -er '.phase' "$receipt")
  sandbox_id=$(jq -er '.sandboxId // empty' "$receipt")
  workspace=$(jq -er '.workspaceId' "$receipt")
  source_commit=$(jq -er '.sourceCommit' "$receipt")
  architecture=$(jq -er '.architecture' "$receipt")
  template_reference=$(jq -er '.template' "$receipt")
  expires=$(jq -er '.expires' "$receipt")
  request_prefix=$(jq -er '.requestPrefix' "$receipt")
  created_at=$(jq -er '.createdAt' "$receipt")
}

resolve_blazn() {
  if [ -z "$blazn" ]; then blazn=$(command -v blazn || true); fi
  [ -n "$blazn" ] && [ -x "$blazn" ] || { printf '%s\n' 'Blazn CLI is not executable; pass --blazn PATH or add blazn to PATH' >&2; exit 1; }
}

require_session() {
  load_receipt
  [ -n "$sandbox_id" ] || { printf 'session creation is incomplete; rerun start with the same receipt: %s\n' "$receipt" >&2; exit 1; }
  [ "$phase" != deleted ] || { printf 'development session is already deleted: %s\n' "$sandbox_id" >&2; exit 1; }
  resolve_blazn
}

require_ready() {
  require_session
  [ "$phase" = ready ] || { printf 'development session is not ready (phase %s): %s\n' "$phase" "$sandbox_id" >&2; exit 1; }
}

prove_status_identity() {
  status_file=$1
  jq -e --arg id "$sandbox_id" '.id == $id' "$status_file" >/dev/null || {
    printf 'sandbox status identity mismatch; expected %s\n' "$sandbox_id" >&2
    return 1
  }
}

capture_patch() {
  patch_output=$1
  # Variables in this command are intentionally expanded inside the Sandbox.
  # shellcheck disable=SC2016
  "$blazn" --output json sandbox exec "$sandbox_id" -- sh -lc 'set -eu; cd /workspace/src/blazn; git -c safe.directory=/workspace/src/blazn add -A; git -c safe.directory=/workspace/src/blazn diff --cached --binary refs/blazn/baseline -- > /workspace/artifacts/change.patch; if test -s /workspace/artifacts/change.patch; then printf PATCH_READY; else rm -f /workspace/artifacts/change.patch; printf NO_CHANGES; fi' >"$evidence/patch.json"
  jq -e '.remoteExitCode == 0 and .truncated == false' "$evidence/patch.json" >/dev/null
  patch_state=$(jq -er '.stdoutBase64' "$evidence/patch.json" | decode_base64)
  if [ "$patch_state" = NO_CHANGES ]; then
    printf '%s\n' 'no source changes; no patch created'
    return 0
  fi
  [ "$patch_state" = PATCH_READY ] || { printf 'unexpected patch generation response: %s\n' "$patch_state" >&2; return 1; }
  sh "$script_dir/download-patch.sh" "$blazn" "$sandbox_id" "$patch_output" "$evidence/patch-download.json" >/dev/null
  printf 'patch downloaded and verified: %s\n' "$patch_output"
}

case $action in
  start)
    workspace=${BLAZN_WORKSPACE_ID:-}
    source_commit=
    architecture=amd64
    expires=2h
    while [ "$#" -gt 0 ]; do
      case $1 in
        --workspace) [ "$#" -ge 2 ] || { printf '%s\n' '--workspace requires a value' >&2; exit 64; }; workspace=$2; shift 2 ;;
        --source) [ "$#" -ge 2 ] || { printf '%s\n' '--source requires a value' >&2; exit 64; }; source_commit=$2; shift 2 ;;
        --arch) [ "$#" -ge 2 ] || { printf '%s\n' '--arch requires a value' >&2; exit 64; }; architecture=$2; shift 2 ;;
        --expires) [ "$#" -ge 2 ] || { printf '%s\n' '--expires requires a value' >&2; exit 64; }; expires=$2; shift 2 ;;
        *) printf 'unknown start option: %s\n' "$1" >&2; exit 64 ;;
      esac
    done
    [ -n "$workspace" ] || { printf '%s\n' 'workspace is required' >&2; exit 64; }
    case $workspace in ????????-????-????-????-????????????) ;; *) printf '%s\n' 'workspace must be a UUID' >&2; exit 64 ;; esac
    case $architecture in amd64|arm64) ;; *) printf '%s\n' 'architecture must be amd64 or arm64' >&2; exit 64 ;; esac
    seconds=$(duration_seconds "$expires") || { printf '%s\n' 'expires must be a duration such as 30m or 2h' >&2; exit 64; }
    [ "$seconds" -le 7200 ] || { printf '%s\n' 'expires must not exceed 2h (7200 seconds)' >&2; exit 64; }
    resolve_blazn
    template_file=$repo_root/examples/coding-agent/sandbox-template-dev.yaml
    template_reference=$(jq -er '.metadata.name + "@" + .spec.version' "$template_file")
    resume_phase=
    if [ -f "$receipt" ]; then
      load_receipt
      case $phase in
        creating|starting) resume_phase=$phase ;;
        *) printf 'refusing to replace existing session receipt in phase %s: %s\n' "$phase" "$receipt" >&2; exit 1 ;;
      esac
      printf 'resuming %s request from %s\n' "$phase" "$receipt" >&2
    else
      if [ -z "$source_commit" ]; then
        [ -z "$(git -C "$repo_root" status --porcelain --untracked-files=normal)" ] || { printf '%s\n' 'working tree is dirty; commit and push it before starting' >&2; exit 1; }
        source_commit=$(git -C "$repo_root" rev-parse --verify HEAD)
      fi
      case $source_commit in *[!0-9a-f]*|'') printf '%s\n' 'source commit must be lowercase hexadecimal' >&2; exit 64 ;; esac
      case ${#source_commit} in 40|64) ;; *) printf '%s\n' 'source commit must contain 40 or 64 hexadecimal characters' >&2; exit 64 ;; esac
      if [ "${BLAZN_SOURCE_PREFLIGHT_FETCH:-1}" = 1 ]; then git -C "$repo_root" fetch --quiet --no-tags origin; fi
      git -C "$repo_root" cat-file -e "$source_commit^{commit}" 2>/dev/null || { printf 'source commit is not present locally: %s\n' "$source_commit" >&2; exit 1; }
      origin_ref=$(git -C "$repo_root" for-each-ref --format='%(refname)' --contains "$source_commit" refs/remotes/origin/ | head -1)
      if [ -z "$origin_ref" ]; then
        git -C "$repo_root" ls-remote --heads --tags origin | awk -v commit="$source_commit" '$1 == commit { found=1 } END { exit found ? 0 : 1 }' || { printf 'source commit is not pushed: %s\n' "$source_commit" >&2; exit 1; }
      fi
      phase=creating
      sandbox_id=
      created_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
      request_prefix=development-session-$(date -u '+%Y%m%d%H%M%S')-$$
      write_receipt
    fi
    mkdir -p -- "$evidence"
    if [ "$resume_phase" != starting ]; then
      "$read_retry" "$evidence/create.json" "$blazn" --output json sandbox create --template "$template_reference" --arch "$architecture" --mode direct --expires "$expires" --approved-non-sensitive --workspace "$workspace" --source "source=$source_commit" --request-id "$request_prefix-create"
      sandbox_id=$(jq -er '.sandbox.id' "$evidence/create.json")
      phase=starting
      write_receipt
    fi
    attempt=1
    while [ "$attempt" -le "${BLAZN_SESSION_READY_ATTEMPTS:-120}" ]; do
      "$read_retry" "$evidence/status.json" "$blazn" --output json sandbox get "$sandbox_id"
      prove_status_identity "$evidence/status.json"
      state=$(jq -er '.state' "$evidence/status.json")
      [ "$state" = ready ] && break
      case $state in failed|recovery_required|deleted) printf 'Sandbox entered terminal create state: %s\n' "$state" >&2; exit 1 ;; esac
      attempt=$((attempt + 1)); sleep "${BLAZN_SESSION_POLL_DELAY_SECONDS:-5}"
    done
    [ "${state:-}" = ready ] || { printf '%s\n' 'Sandbox did not reach ready state' >&2; exit 1; }
    "$blazn" --output json sandbox exec "$sandbox_id" -- sh -lc 'set -eu; cd /workspace/src/blazn; rm -rf .git; git init -q; git -c safe.directory=/workspace/src/blazn add -A; git -c safe.directory=/workspace/src/blazn -c user.name=Blazn -c user.email=development@blazn.invalid -c commit.gpgsign=false commit -qm "Blazn materialized source baseline"; git -c safe.directory=/workspace/src/blazn update-ref refs/blazn/baseline HEAD' >"$evidence/baseline.json"
    jq -e '.remoteExitCode == 0 and .truncated == false' "$evidence/baseline.json" >/dev/null
    phase=ready
    write_receipt
    printf 'session ready: %s\nreceipt: %s\n' "$sandbox_id" "$receipt"
    ;;
  status)
    [ "$#" -eq 0 ] || { printf '%s\n' 'status takes no arguments' >&2; exit 64; }
    load_receipt
    if [ "$phase" = deleted ]; then jq . "$receipt"; exit 0; fi
    [ -n "$sandbox_id" ] || { jq . "$receipt"; exit 0; }
    resolve_blazn
    "$read_retry" "$evidence/status.json" "$blazn" --output json sandbox get "$sandbox_id"
    prove_status_identity "$evidence/status.json"
    jq --arg receipt "$receipt" --arg sourceCommit "$source_commit" '. + {receipt:$receipt, sourceCommit:$sourceCommit}' "$evidence/status.json"
    ;;
  exec)
    require_ready
    [ "${1:-}" = -- ] && shift
    [ "$#" -gt 0 ] || { printf '%s\n' 'exec requires a command after --' >&2; exit 64; }
    if "$blazn" --output json sandbox exec "$sandbox_id" -- "$@" >"$evidence/exec.json"; then exec_status=0; else exec_status=$?; fi
    if jq -e '.stdoutBase64 and .stderrBase64' "$evidence/exec.json" >/dev/null 2>&1; then
      jq -r '.stdoutBase64' "$evidence/exec.json" | decode_base64
      jq -r '.stderrBase64' "$evidence/exec.json" | decode_base64 >&2
    else
      cat "$evidence/exec.json"
    fi
    exit "$exec_status"
    ;;
  upload)
    require_ready
    [ "$#" -eq 2 ] || { printf '%s\n' 'upload requires LOCAL_PATH REMOTE_PATH' >&2; exit 64; }
    [ -f "$1" ] || { printf 'upload source must be a regular file: %s\n' "$1" >&2; exit 1; }
    upload_size=$(wc -c <"$1" | tr -d ' ')
    [ "$upload_size" -le 8388608 ] || { printf '%s\n' 'upload exceeds the 8 MiB transfer limit' >&2; exit 1; }
    "$blazn" --output json sandbox upload "$sandbox_id" "$1" "$2"
    ;;
  download)
    require_ready
    [ "$#" -eq 2 ] || { printf '%s\n' 'download requires REMOTE_PATH LOCAL_PATH' >&2; exit 64; }
    [ ! -e "$2" ] || { printf 'refusing to overwrite local path: %s\n' "$2" >&2; exit 1; }
    download_parent=$(dirname -- "$2")
    [ -d "$download_parent" ] && [ -w "$download_parent" ] || { printf 'download directory is not writable: %s\n' "$download_parent" >&2; exit 1; }
    download_parent=$(CDPATH='' cd -- "$download_parent" && pwd)
    download_target=$download_parent/$(basename -- "$2")
    download_temp=$(mktemp "$download_parent/.blazn-download.XXXXXX")
    rm -f -- "$download_temp"
    cleanup_file=$download_temp
    "$blazn" --output json sandbox download "$sandbox_id" "$1" "$download_temp" >"$evidence/download.json"
    download_size=$(wc -c <"$download_temp" | tr -d ' ')
    [ "$download_size" -le 8388608 ] || { printf '%s\n' 'download exceeds the 8 MiB transfer limit' >&2; exit 1; }
    ln -- "$download_temp" "$download_target" 2>/dev/null || { printf 'refusing to overwrite local path: %s\n' "$download_target" >&2; exit 1; }
    rm -f -- "$download_temp"
    cleanup_file=
    cat "$evidence/download.json"
    ;;
  patch)
    require_ready
    [ "$#" -eq 1 ] || { printf '%s\n' 'patch requires OUTPUT_PATH' >&2; exit 64; }
    capture_patch "$1"
    ;;
  finish)
    require_session
    if [ "$phase" = ready ]; then
      case ${1:-} in
        --patch)
          [ "$#" -eq 2 ] || { printf '%s\n' 'finish --patch requires OUTPUT_PATH' >&2; exit 64; }
          capture_patch "$2"
          phase=patch_saved
          write_receipt
          ;;
        --discard)
          [ "$#" -eq 1 ] || { printf '%s\n' 'finish --discard takes no value' >&2; exit 64; }
          phase=discard_approved
          write_receipt
          ;;
        *) printf '%s\n' 'finish requires --patch OUTPUT_PATH or explicit --discard' >&2; exit 64 ;;
      esac
    elif [ "$phase" = starting ]; then
      [ "${1:-}" = --discard ] && [ "$#" -eq 1 ] || { printf '%s\n' 'an incomplete start may only be finished with explicit --discard' >&2; exit 64; }
      phase=discard_approved
      write_receipt
    else
      [ "$#" -eq 0 ] || { printf 'resuming finish from phase %s; do not repeat finish options\n' "$phase" >&2; exit 64; }
      case $phase in patch_saved|discard_approved|stopping|deleting) ;; *) printf 'cannot finish session in phase %s\n' "$phase" >&2; exit 1 ;; esac
    fi
    if [ "$phase" != deleting ]; then
      "$read_retry" "$evidence/pre-stop.json" "$blazn" --output json sandbox get "$sandbox_id"
      prove_status_identity "$evidence/pre-stop.json"
      state=$(jq -er '.state' "$evidence/pre-stop.json")
      desired_state=$(jq -er '.desiredState' "$evidence/pre-stop.json")
      if [ "$state" = deleted ] && [ "$desired_state" = deleted ]; then
        phase=deleted
        write_receipt
        printf 'session expiry cleanup proven: %s\n' "$sandbox_id"
        exit 0
      fi
      phase=stopping
      write_receipt
      "$blazn" --output json sandbox stop "$sandbox_id" --request-id "$request_prefix-stop" >"$evidence/stop.json"
      attempt=1
      while [ "$attempt" -le "${BLAZN_SESSION_STOP_ATTEMPTS:-120}" ]; do
        "$read_retry" "$evidence/stopped.json" "$blazn" --output json sandbox get "$sandbox_id"
        prove_status_identity "$evidence/stopped.json"
        state=$(jq -er '.state' "$evidence/stopped.json")
        [ "$state" = stopped ] && break
        if [ "$state" = deleted ]; then
          desired_state=$(jq -er '.desiredState' "$evidence/stopped.json")
          if [ "$desired_state" = deleted ]; then
            phase=deleted
            write_receipt
            printf 'session expiry cleanup proven: %s\n' "$sandbox_id"
            exit 0
          fi
        fi
        case $state in failed|recovery_required|deleted) printf 'Sandbox entered unexpected stop state: %s\n' "$state" >&2; exit 1 ;; esac
        attempt=$((attempt + 1)); sleep "${BLAZN_SESSION_POLL_DELAY_SECONDS:-2}"
      done
      [ "${state:-}" = stopped ] || { printf '%s\n' 'Sandbox did not reach stopped state' >&2; exit 1; }
      phase=deleting
      write_receipt
    fi
    sh "$delete_and_prove" "$read_retry" "$blazn" "$sandbox_id" "$request_prefix-delete" "$evidence"
    phase=deleted
    write_receipt
    printf 'session deleted and cleanup proven: %s\n' "$sandbox_id"
    ;;
  *) printf 'unknown command: %s\n' "$action" >&2; usage >&2; exit 64 ;;
esac

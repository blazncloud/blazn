#!/bin/sh
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/../.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/blazn-dev-session-test.XXXXXX")
source_commit=
test_ref=
ref_created=0
cleanup() {
  status=$?
  trap - EXIT HUP INT TERM
  if [ "$ref_created" -eq 1 ]; then git -C "$repo_root" update-ref -d "$test_ref" "$source_commit" || status=1; fi
  rm -r -- "$work" || status=1
  exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
source_commit=$(git -C "$repo_root" rev-parse HEAD)
test_ref=refs/remotes/origin/blazn-development-session-shell-test-$$
if ! git -C "$repo_root" update-ref "$test_ref" "$source_commit" 0000000000000000000000000000000000000000; then
  printf 'unable to create isolated pushed-source test ref: %s\n' "$test_ref" >&2
  exit 1
fi
ref_created=1
receipt=$work/session.json
state=$work/fake-state
log=$work/fake.log
workspace=3340c6d2-3684-4580-8385-146f1f11220c

cat >"$work/blazn" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$FAKE_LOG"
args=" $* "
case $args in
  *' sandbox create '*) printf '%s\n' '{"sandbox":{"id":"66578829-ee27-49b1-bfc0-65813042ceaf"}}' ;;
  *' sandbox get '*)
    phase=$(cat "$FAKE_STATE" 2>/dev/null || printf ready)
    printf '{"id":"%s","state":"%s","desiredState":"%s"}\n' "${FAKE_GET_ID:-66578829-ee27-49b1-bfc0-65813042ceaf}" "$phase" "$phase"
    ;;
  *' sandbox stop '*) printf stopped >"$FAKE_STATE"; printf '%s\n' '{}' ;;
  *' sandbox delete '*) printf deleted >"$FAKE_STATE"; printf '%s\n' '{}' ;;
  *' sandbox exec '*' diff --cached '*) printf '%s\n' '{"remoteExitCode":0,"truncated":false,"stdoutBase64":"UEFUQ0hfUkVBRFk=","stderrBase64":""}' ;;
  *' sandbox exec '*) printf '%s\n' '{"remoteExitCode":0,"truncated":false,"stdoutBase64":"","stderrBase64":""}' ;;
  *' sandbox upload '*) printf '%s\n' '{}' ;;
  *' sandbox download '*)
    count=$#
    eval "destination=\${$count}"
    printf '%s\n' 'diff --git a/README.md b/README.md' >"$destination"
    if [ -n "${FAKE_RACE_TARGET:-}" ]; then printf '%s\n' race >"$FAKE_RACE_TARGET"; fi
    if command -v sha256sum >/dev/null 2>&1; then digest=$(sha256sum "$destination" | awk '{print $1}'); else digest=$(shasum -a 256 "$destination" | awk '{print $1}'); fi
    printf '{"sha256":"sha256:%s"}\n' "$digest"
    ;;
  *) printf 'unexpected fake invocation: %s\n' "$*" >&2; exit 1 ;;
esac
EOF
chmod +x "$work/blazn"
export FAKE_LOG="$log" FAKE_STATE="$state" BLAZN_SOURCE_PREFLIGHT_FETCH=0

if "$script_dir/dev-session.sh" --receipt "$work/huge.json" --blazn "$work/blazn" start --workspace "$workspace" --source "$source_commit" --expires 9223372036854775808h >"$work/huge.out" 2>"$work/huge.err"; then
  printf '%s\n' 'huge duration unexpectedly bypassed the two-hour cap' >&2
  exit 1
fi
grep -F 'expires must be a duration' "$work/huge.err" >/dev/null

"$script_dir/dev-session.sh" --receipt "$receipt" --blazn "$work/blazn" start --workspace "$workspace" --source "$source_commit" --expires 30m >"$work/start.out"
jq -e '.phase == "ready" and .sandboxId == "66578829-ee27-49b1-bfc0-65813042ceaf" and .expires == "30m"' "$receipt" >/dev/null
grep -F 'safe.directory=/workspace/src/blazn' "$log" >/dev/null
[ "$(stat -f '%Lp' "$receipt" 2>/dev/null || stat -c '%a' "$receipt")" = 600 ]

resume_receipt=$work/resume-session.json
jq '.phase = "starting"' "$receipt" >"$resume_receipt"
chmod 600 "$resume_receipt"
: >"$log"
"$script_dir/dev-session.sh" --receipt "$resume_receipt" --blazn "$work/blazn" start --workspace "$workspace" --source "$source_commit" --expires 30m >"$work/resume-start.out"
jq -e '.phase == "ready"' "$resume_receipt" >/dev/null
if grep -F 'sandbox create' "$log" >/dev/null; then
  printf '%s\n' 'starting-phase resume unexpectedly replayed create' >&2
  exit 1
fi

expired_receipt=$work/expired-session.json
cp "$resume_receipt" "$expired_receipt"
mkdir "$expired_receipt.evidence"
printf deleted >"$state"
: >"$log"
"$script_dir/dev-session.sh" --receipt "$expired_receipt" --blazn "$work/blazn" finish --discard >"$work/expired-finish.out"
jq -e '.phase == "deleted"' "$expired_receipt" >/dev/null
if grep -F 'sandbox stop' "$log" >/dev/null; then
  printf '%s\n' 'expired session unexpectedly issued stop after deletion proof' >&2
  exit 1
fi
printf ready >"$state"

mkdir "$receipt.lock"
if "$script_dir/dev-session.sh" --receipt "$receipt" --blazn "$work/blazn" status >"$work/locked.out" 2>"$work/locked.err"; then
  printf '%s\n' 'receipt lock unexpectedly allowed a concurrent command' >&2
  exit 1
fi
rmdir "$receipt.lock"
grep -F 'development session receipt is busy' "$work/locked.err" >/dev/null

export FAKE_GET_ID=77578829-ee27-49b1-bfc0-65813042ceaf
if "$script_dir/dev-session.sh" --receipt "$receipt" --blazn "$work/blazn" status >"$work/mismatch.out" 2>"$work/mismatch.err"; then
  printf '%s\n' 'status unexpectedly accepted a mismatched sandbox identity' >&2
  exit 1
fi
unset FAKE_GET_ID
grep -F 'sandbox status identity mismatch' "$work/mismatch.err" >/dev/null

"$script_dir/dev-session.sh" --receipt "$receipt" --blazn "$work/blazn" status | jq -e '.state == "ready"' >/dev/null
"$script_dir/dev-session.sh" --receipt "$receipt" --blazn "$work/blazn" exec -- go test ./... >/dev/null
printf upload >"$work/local.txt"
"$script_dir/dev-session.sh" --receipt "$receipt" --blazn "$work/blazn" upload "$work/local.txt" /workspace/artifacts/local.txt >/dev/null
"$script_dir/dev-session.sh" --receipt "$receipt" --blazn "$work/blazn" download /workspace/artifacts/remote.txt "$work/download.txt" >/dev/null
export FAKE_RACE_TARGET="$work/raced-download.txt"
if "$script_dir/dev-session.sh" --receipt "$receipt" --blazn "$work/blazn" download /workspace/artifacts/remote.txt "$work/raced-download.txt" >"$work/race.out" 2>"$work/race.err"; then
  printf '%s\n' 'download unexpectedly overwrote a concurrently created target' >&2
  exit 1
fi
[ "$(cat "$work/raced-download.txt")" = race ]
unset FAKE_RACE_TARGET
"$script_dir/dev-session.sh" --receipt "$receipt" --blazn "$work/blazn" patch "$work/change.patch" >/dev/null
test -s "$work/change.patch"
test -s "$work/change.patch.sha256"
if "$script_dir/dev-session.sh" --receipt "$receipt" --blazn "$work/blazn" finish >"$work/unsafe-finish.out" 2>"$work/unsafe-finish.err"; then
  printf '%s\n' 'finish unexpectedly accepted teardown without patch or discard' >&2
  exit 1
fi
grep -F 'finish requires --patch OUTPUT_PATH or explicit --discard' "$work/unsafe-finish.err" >/dev/null
BLAZN_DELETE_POLL_DELAY_SECONDS=0 BLAZN_SESSION_POLL_DELAY_SECONDS=0 "$script_dir/dev-session.sh" --receipt "$receipt" --blazn "$work/blazn" finish --patch "$work/final.patch" >"$work/finish.out"
test -s "$work/final.patch"
test -s "$work/final.patch.sha256"
jq -e '.phase == "deleted" and .sandboxId == "66578829-ee27-49b1-bfc0-65813042ceaf"' "$receipt" >/dev/null
"$script_dir/dev-session.sh" --receipt "$receipt" --blazn "$work/blazn" status | jq -e '.phase == "deleted"' >/dev/null
grep -F 'sandbox delete 66578829-ee27-49b1-bfc0-65813042ceaf' "$log" >/dev/null

if "$script_dir/dev-session.sh" --receipt "$receipt" --blazn "$work/blazn" exec -- true >"$work/deleted.out" 2>"$work/deleted.err"; then
  printf '%s\n' 'exec unexpectedly accepted a deleted session' >&2
  exit 1
fi
grep -F 'development session is already deleted' "$work/deleted.err" >/dev/null

printf '%s\n' 'development session tests passed'

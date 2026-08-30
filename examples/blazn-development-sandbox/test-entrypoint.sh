#!/bin/sh
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/blazn-development-entrypoint.XXXXXX")
dirty_marker=$script_dir/.entrypoint-dirty-test.$$
default_one=
default_two=
cleanup() {
  rm -f -- "$dirty_marker"
  [ -z "$default_one" ] || rmdir -- "$default_one" 2>/dev/null || true
  [ -z "$default_two" ] || rmdir -- "$default_two" 2>/dev/null || true
  rm -r -- "$work"
}
trap cleanup EXIT HUP INT TERM

mock_blazn=$work/blazn
mock_runner=$work/runner
printf '%s\n' '#!/bin/sh' 'exit 0' >"$mock_blazn"
cat >"$mock_runner" <<'EOF'
#!/bin/sh
printf '%s\n' "$@" >"$ENTRYPOINT_CAPTURE"
printf '%s\n' "${BLAZN_SKIP_TEMPLATE_PUBLISH:-0}" >"$ENTRYPOINT_SKIP_CAPTURE"
printf '%s\n' "${BLAZN_DEVELOPMENT_PATCH_OUTPUT:-}" >"$ENTRYPOINT_PATCH_CAPTURE"
EOF
chmod +x "$mock_blazn" "$mock_runner"

capture=$work/arguments
skip_capture=$work/skip
patch_capture=$work/patch
workspace=40000000-0000-4000-8000-000000000001
commit=$(git -C "$script_dir/../.." rev-parse origin/main)
ENTRYPOINT_CAPTURE=$capture ENTRYPOINT_SKIP_CAPTURE=$skip_capture ENTRYPOINT_PATCH_CAPTURE=$patch_capture \
  BLAZN_SOURCE_PREFLIGHT_FETCH=0 BLAZN_DEVELOPMENT_ACCEPTANCE_RUNNER=$mock_runner \
  "$script_dir/run-live.sh" --workspace "$workspace" --blazn "$mock_blazn" \
    --source "$commit" --arch arm64 --patch-output "$work/result.patch" >/dev/null

template_file=$(CDPATH='' cd -- "$script_dir/../coding-agent" && pwd)/sandbox-template-dev.yaml
expected=$work/expected
printf '%s\n' \
  "$mock_blazn" \
  "$workspace" \
  "$template_file" \
  'coding-agent@go-1.26.2-node-22.19.0-poc-dev-5' \
  "$commit" \
  arm64 >"$expected"
cmp "$expected" "$capture"
[ "$(cat "$skip_capture")" = 1 ]
[ "$(cat "$patch_capture")" = "$work/result.patch" ]

ENTRYPOINT_CAPTURE=$capture ENTRYPOINT_SKIP_CAPTURE=$skip_capture ENTRYPOINT_PATCH_CAPTURE=$patch_capture \
  BLAZN_SKIP_TEMPLATE_PUBLISH=1 BLAZN_DEVELOPMENT_ACCEPTANCE_RUNNER=$mock_runner \
  BLAZN_SOURCE_PREFLIGHT_FETCH=0 \
  "$script_dir/run-live.sh" --workspace "$workspace" --blazn "$mock_blazn" \
    --source "$commit" --publish-template >/dev/null
[ "$(tail -1 "$capture")" = amd64 ]
[ "$(cat "$skip_capture")" = 0 ]
default_one=$(dirname -- "$(cat "$patch_capture")")
case $default_one in "${TMPDIR:-/tmp}"/blazn-development-output.*) ;; *) printf 'unsafe default patch directory: %s\n' "$default_one" >&2; exit 1 ;; esac

ENTRYPOINT_CAPTURE=$capture ENTRYPOINT_SKIP_CAPTURE=$skip_capture ENTRYPOINT_PATCH_CAPTURE=$patch_capture \
  BLAZN_SOURCE_PREFLIGHT_FETCH=0 BLAZN_DEVELOPMENT_ACCEPTANCE_RUNNER=$mock_runner \
  "$script_dir/run-live.sh" --workspace "$workspace" --blazn "$mock_blazn" \
    --source "$commit" >/dev/null
default_two=$(dirname -- "$(cat "$patch_capture")")
[ "$default_one" != "$default_two" ]
case $default_two in "${TMPDIR:-/tmp}"/blazn-development-output.*) ;; *) printf 'unsafe repeated patch directory: %s\n' "$default_two" >&2; exit 1 ;; esac

: >"$dirty_marker"
if ENTRYPOINT_CAPTURE=$capture ENTRYPOINT_SKIP_CAPTURE=$skip_capture ENTRYPOINT_PATCH_CAPTURE=$patch_capture \
  BLAZN_SOURCE_PREFLIGHT_FETCH=0 BLAZN_DEVELOPMENT_ACCEPTANCE_RUNNER=$mock_runner \
  "$script_dir/run-live.sh" --workspace "$workspace" --blazn "$mock_blazn" --arch amd64 >"$work/dirty.out" 2>"$work/dirty.err"; then
  printf '%s\n' 'entrypoint unexpectedly accepted a dirty default HEAD' >&2
  exit 1
fi
rm -f -- "$dirty_marker"
grep -F 'working tree is dirty or has untracked files' "$work/dirty.err" >/dev/null

rm -f -- "$capture"
if ENTRYPOINT_CAPTURE=$capture ENTRYPOINT_SKIP_CAPTURE=$skip_capture ENTRYPOINT_PATCH_CAPTURE=$patch_capture \
  BLAZN_SOURCE_PREFLIGHT_FETCH=0 BLAZN_DEVELOPMENT_ACCEPTANCE_RUNNER=$mock_runner \
  "$script_dir/run-live.sh" --workspace "$workspace" --blazn "$mock_blazn" \
    --source 0000000000000000000000000000000000000000 >"$work/unreachable.out" 2>"$work/unreachable.err"; then
  printf '%s\n' 'entrypoint unexpectedly accepted an unavailable source commit' >&2
  exit 1
fi
[ ! -e "$capture" ]
grep -F 'source commit is not present locally' "$work/unreachable.err" >/dev/null

if BLAZN_DEVELOPMENT_ACCEPTANCE_RUNNER=$mock_runner \
  "$script_dir/run-live.sh" --blazn "$mock_blazn" --source "$commit" --arch amd64 >"$work/missing.out" 2>"$work/missing.err"; then
  printf '%s\n' 'entrypoint unexpectedly accepted a missing workspace' >&2
  exit 1
fi
grep -F 'workspace is required' "$work/missing.err" >/dev/null

"$script_dir/run-live.sh" --help | grep -F 'BLAZN_WORKSPACE_ID' >/dev/null
printf '%s\n' 'Blazn development entrypoint checks passed.'

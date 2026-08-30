#!/bin/sh
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/blazn-development-delete.XXXXXX")
cleanup() { rm -r -- "$work"; }
trap cleanup EXIT HUP INT TERM

sandbox=10000000-0000-4000-8000-000000000001
request=development-cleanup-test
mock=$work/blazn
cat >"$mock" <<'EOF'
#!/bin/sh
command=$4
if [ "$command" = delete ]; then
  printf '%s\n' "$7" >>"$REQUEST_LOG"
  count=0
  [ ! -f "$DELETE_COUNT" ] || count=$(cat "$DELETE_COUNT")
  count=$((count + 1))
  printf '%s\n' "$count" >"$DELETE_COUNT"
  [ "$count" -gt 1 ] || exit 7
  printf '{"accepted":true}\n'
  exit 0
fi
count=0
[ ! -f "$GET_COUNT" ] || count=$(cat "$GET_COUNT")
count=$((count + 1))
printf '%s\n' "$count" >"$GET_COUNT"
state=deleting
[ "$count" -lt 2 ] || state=deleted
printf '{"id":"%s","state":"%s","desiredState":"deleted"}\n' "${RETURN_ID:-$5}" "$state"
EOF
chmod +x "$mock"
mkdir "$work/success"
REQUEST_LOG=$work/requests DELETE_COUNT=$work/delete-count GET_COUNT=$work/get-count \
  BLAZN_READ_RETRY_DELAY_SECONDS=0 BLAZN_DELETE_POLL_DELAY_SECONDS=0 \
  sh "$script_dir/delete-and-prove.sh" "$script_dir/read-retry.sh" "$mock" \
    "$sandbox" "$request" "$work/success"
[ "$(cat "$work/delete-count")" = 2 ]
[ "$(sort -u "$work/requests")" = "$request" ]
[ "$(cat "$work/get-count")" = 2 ]

mkdir "$work/mismatch"
if REQUEST_LOG=$work/mismatch-requests DELETE_COUNT=$work/mismatch-delete GET_COUNT=$work/mismatch-get \
  RETURN_ID=20000000-0000-4000-8000-000000000002 \
  BLAZN_READ_RETRY_DELAY_SECONDS=0 BLAZN_DELETE_POLL_DELAY_SECONDS=0 \
  sh "$script_dir/delete-and-prove.sh" "$script_dir/read-retry.sh" "$mock" \
    "$sandbox" "$request" "$work/mismatch" >"$work/mismatch.out" 2>"$work/mismatch.err"; then
  printf '%s\n' 'cleanup unexpectedly accepted a mismatched Sandbox' >&2
  exit 1
fi
grep -F 'different Sandbox' "$work/mismatch.err" >/dev/null
printf '%s\n' 'Blazn development deletion proof checks passed.'

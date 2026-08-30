#!/bin/sh
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/blazn-development-read-retry.XXXXXX")
cleanup() { rm -r -- "$work"; }
trap cleanup EXIT HUP INT TERM

fake=$work/fake
cat >"$fake" <<'EOF'
#!/bin/sh
case ${SELF_SIGNAL:-} in
  HUP|INT|TERM) printf 'partial signal response\n'; kill -"$SELF_SIGNAL" "$PPID"; exit 0 ;;
esac
[ -z "${RETRY_ARG_LOG:-}" ] || printf '%s\n' "$*" >>"$RETRY_ARG_LOG"
count=0
[ ! -f "$RETRY_COUNT" ] || count=$(cat "$RETRY_COUNT")
count=$((count + 1))
printf '%s\n' "$count" >"$RETRY_COUNT"
if [ "$count" -lt "$SUCCEED_ON" ]; then
  printf 'partial response\n'
  exit "$FAILURE_STATUS"
fi
printf 'complete response\n'
EOF
chmod +x "$fake"

RETRY_COUNT=$work/transient-count RETRY_ARG_LOG=$work/transient-args SUCCEED_ON=3 FAILURE_STATUS=7 \
  BLAZN_READ_RETRY_ATTEMPTS=4 BLAZN_READ_RETRY_DELAY_SECONDS=0 \
  "$script_dir/read-retry.sh" "$work/transient-output" "$fake" \
    sandbox create --request-id fixed-create-request 2>"$work/transient-error"
[ "$(cat "$work/transient-count")" = 3 ]
[ "$(cat "$work/transient-output")" = 'complete response' ]
[ "$(grep -c 'transient read failure' "$work/transient-error")" = 2 ]
[ "$(sort -u "$work/transient-args")" = 'sandbox create --request-id fixed-create-request' ]

if RETRY_COUNT=$work/fatal-count SUCCEED_ON=3 FAILURE_STATUS=1 \
  BLAZN_READ_RETRY_ATTEMPTS=4 BLAZN_READ_RETRY_DELAY_SECONDS=0 \
  "$script_dir/read-retry.sh" "$work/fatal-output" "$fake" 2>"$work/fatal-error"; then
  printf '%s\n' 'non-retryable failure unexpectedly succeeded' >&2
  exit 1
else
  status=$?
fi
[ "$status" -eq 1 ]
[ "$(cat "$work/fatal-count")" = 1 ]
[ ! -e "$work/fatal-output" ]
[ ! -s "$work/fatal-error" ]

for signal_and_status in HUP:129 INT:130 TERM:143; do
  signal=${signal_and_status%:*}
  expected_status=${signal_and_status#*:}
  output=$work/signal-$signal-output
  if RETRY_COUNT=$work/signal-$signal-count SUCCEED_ON=1 FAILURE_STATUS=0 SELF_SIGNAL=$signal \
    "$script_dir/read-retry.sh" "$output" "$fake" >"$work/signal-$signal.out" 2>"$work/signal-$signal.err"; then
    printf 'signal-interrupted retry unexpectedly succeeded for %s\n' "$signal" >&2
    exit 1
  else
    status=$?
  fi
  [ "$status" -eq "$expected_status" ]
  [ ! -e "$output" ]
  if find "$work" -name "signal-$signal-output.retry.*" | grep . >/dev/null; then
    printf 'retry temporary output remained after %s\n' "$signal" >&2
    exit 1
  fi
done

printf '%s\n' 'Blazn development read retry checks passed.'

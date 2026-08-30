#!/bin/sh
set -eu

if [ "$#" -lt 2 ]; then
  printf 'usage: %s OUTPUT_FILE COMMAND [ARGUMENT ...]\n' "$0" >&2
  exit 64
fi

output=$1
shift
attempts=${BLAZN_READ_RETRY_ATTEMPTS:-5}
delay=${BLAZN_READ_RETRY_DELAY_SECONDS:-2}
case $attempts in ''|*[!0-9]*|0) printf '%s\n' 'BLAZN_READ_RETRY_ATTEMPTS must be a positive integer' >&2; exit 64 ;; esac
case $delay in ''|*[!0-9]*) printf '%s\n' 'BLAZN_READ_RETRY_DELAY_SECONDS must be a non-negative integer' >&2; exit 64 ;; esac

temporary=$output.retry.$$
cleanup() { rm -f -- "$temporary"; }
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

attempt=1
while :; do
  if "$@" >"$temporary"; then
    mv -- "$temporary" "$output"
    exit 0
  else
    status=$?
  fi
  if [ "$status" -ne 7 ] || [ "$attempt" -ge "$attempts" ]; then
    exit "$status"
  fi
  printf 'Blazn development E2E: transient read failure (exit 7); retrying %s/%s\n' \
    "$((attempt + 1))" "$attempts" >&2
  attempt=$((attempt + 1))
  [ "$delay" -eq 0 ] || sleep "$delay"
done

#!/bin/sh
set -eu

if [ "$#" -ne 5 ]; then
  printf 'usage: %s RETRY_HELPER BLAZN_BIN SANDBOX_ID REQUEST_ID EVIDENCE_DIR\n' "$0" >&2
  exit 64
fi

retry=$1
blazn=$2
sandbox_id=$3
request_id=$4
evidence=$5
attempts=${BLAZN_DELETE_POLL_ATTEMPTS:-120}
delay=${BLAZN_DELETE_POLL_DELAY_SECONDS:-2}
case $attempts in ''|*[!0-9]*|0) printf '%s\n' 'BLAZN_DELETE_POLL_ATTEMPTS must be a positive integer' >&2; exit 64 ;; esac
case $delay in ''|*[!0-9]*) printf '%s\n' 'BLAZN_DELETE_POLL_DELAY_SECONDS must be a non-negative integer' >&2; exit 64 ;; esac

# Replaying this exact request ID is safe after an ambiguous transport failure.
"$retry" "$evidence/delete.json" "$blazn" --output json sandbox delete \
  "$sandbox_id" --request-id "$request_id"

attempt=1
while [ "$attempt" -le "$attempts" ]; do
  "$retry" "$evidence/deleted.json" "$blazn" --output json sandbox get "$sandbox_id"
  if ! jq -e --arg id "$sandbox_id" '.id == $id' "$evidence/deleted.json" >/dev/null; then
    printf 'ERROR: cleanup lookup returned a different Sandbox than %s\n' "$sandbox_id" >&2
    exit 1
  fi
  state=$(jq -er '.state' "$evidence/deleted.json")
  if [ "$state" = deleted ]; then
    jq -e '.desiredState == "deleted"' "$evidence/deleted.json" >/dev/null
    exit 0
  fi
  case $state in
    failed|recovery_required)
      printf 'ERROR: Sandbox %s entered cleanup state %s\n' "$sandbox_id" "$state" >&2
      exit 1
      ;;
  esac
  attempt=$((attempt + 1))
  [ "$delay" -eq 0 ] || sleep "$delay"
done

printf 'ERROR: deletion of Sandbox %s was not proven after %s polls\n' "$sandbox_id" "$attempts" >&2
exit 1

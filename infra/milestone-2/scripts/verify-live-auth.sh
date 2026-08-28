#!/bin/sh
set -eu

[ "$#" -eq 4 ] || { printf 'usage: verify-live-auth.sh CLI API_URL LOGIN PASSWORD_FILE\n' >&2; exit 2; }
cli=$1
api=${2%/}
login=$3
password_file=$4
[ -x "$cli" ] || { printf 'CLI is not executable\n' >&2; exit 2; }
case "$api" in https://*) ;; *) printf 'API URL must use HTTPS\n' >&2; exit 2 ;; esac
if [ ! -f "$password_file" ] || [ -L "$password_file" ]; then
  printf 'password file is unavailable\n' >&2
  exit 2
fi
command -v curl >/dev/null
command -v jq >/dev/null

work=$(mktemp -d /tmp/blazn-live-auth.XXXXXX)
case "$work" in /tmp/blazn-live-auth.*) ;; *) printf 'unsafe temporary path\n' >&2; exit 1 ;; esac
login_pid=
stream_pid=
cleanup() {
  [ -z "$login_pid" ] || kill "$login_pid" 2>/dev/null || true
  [ -z "$stream_pid" ] || kill "$stream_pid" 2>/dev/null || true
  [ -d "$work" ] && [ ! -L "$work" ] && rm -rf -- "$work"
}
trap cleanup EXIT HUP INT TERM
umask 077
mkdir "$work/home" "$work/data"

export HOME="$work/home" XDG_DATA_HOME="$work/data" BLAZN_API_URL="$api"
"$cli" auth login --no-browser >"$work/login.out" 2>"$work/login.err" &
login_pid=$!
code=
attempt=0
while [ "$attempt" -lt 100 ]; do
  code=$(sed -n 's/.* enter code \([A-HJ-NP-Z2-9][A-HJ-NP-Z2-9-]*\)$/\1/p' "$work/login.out" | head -1)
  [ -z "$code" ] || break
  kill -0 "$login_pid" 2>/dev/null || break
  attempt=$((attempt + 1))
  sleep 0.1
done
[ -n "$code" ] || { sed -n '1,20p' "$work/login.err" >&2; printf 'device code was not emitted\n' >&2; exit 1; }

password=$(tr -d '\r\n' <"$password_file")
[ -n "$password" ] || { printf 'password file is empty\n' >&2; exit 1; }
printf 'data-urlencode = "password=%s"\n' "$password" >"$work/password.curl"
unset password
curl --fail --silent --show-error \
  --data-urlencode "user_code=$code" \
  --data-urlencode "email=$login" \
  --config "$work/password.curl" \
  "$api/v1/auth/device/approve" >"$work/approval.html"

wait "$login_pid"
login_pid=
grep -F 'Authenticated as ' "$work/login.out" >/dev/null
"$cli" --output json auth status >"$work/status.json"
jq -e '.authenticated == true and .user.status == "active" and .device.status == "active"' "$work/status.json" >/dev/null
device_id=$(jq -r '.device.id' "$work/status.json")

# The CLI resolves the credential directory from user.Current().HomeDir (which
# with the CGO_ENABLED=0 release binary is $HOME), not XDG_DATA_HOME, and names
# the file session-<origin digest>.json. Glob for it instead of guessing the
# digest.
cred_dir=$HOME/.local/share/blazn/credentials
credential=
for candidate in "$cred_dir"/session-*.json; do
  [ -e "$candidate" ] || continue
  if [ -n "$credential" ]; then
    printf 'multiple credential files present in %s\n' "$cred_dir" >&2
    exit 1
  fi
  credential=$candidate
done
if [ -z "$credential" ]; then
  # The protected file may be legitimately absent when the Linux Secret Service
  # backend was selected; distinguish that from a genuine failure.
  backend=
  for receipt in "$cred_dir"/session-*.backend; do
    [ -e "$receipt" ] || continue
    backend=$(tr -d '\r\n' <"$receipt")
    break
  done
  if [ "$backend" = secret-service ]; then
    printf 'credential is stored via the Secret Service backend, so no protected file exists; this live check requires the protected-file backend (run it without an active Secret Service / secret-tool)\n' >&2
    exit 2
  fi
  printf 'credential file is missing\n' >&2
  exit 1
fi
if [ ! -f "$credential" ] || [ -L "$credential" ] || [ "$(stat -c '%a' "$credential")" != 600 ]; then
  printf 'credential file is unsafe\n' >&2
  exit 1
fi
access_token=$(jq -r '.accessToken' "$credential")
printf 'header = "Authorization: Bearer %s"\n' "$access_token" >"$work/auth.curl"
unset access_token
curl --fail --silent --show-error --no-buffer --config "$work/auth.curl" "$api/v1/events" >"$work/events" &
stream_pid=$!
attempt=0
while [ "$attempt" -lt 50 ]; do
  grep -F 'event: ready' "$work/events" >/dev/null 2>&1 && break
  attempt=$((attempt + 1))
  sleep 0.1
done
grep -F 'event: ready' "$work/events" >/dev/null

"$cli" auth revoke-device "$device_id" >/dev/null
attempt=0
while kill -0 "$stream_pid" 2>/dev/null && [ "$attempt" -lt 50 ]; do
  attempt=$((attempt + 1))
  sleep 0.1
done
kill -0 "$stream_pid" 2>/dev/null && { printf 'revoked event stream remained open\n' >&2; exit 1; }
wait "$stream_pid" || true
stream_pid=
grep -F 'event: revoked' "$work/events" >/dev/null

status=$(curl --silent --output "$work/revoked.json" --write-out '%{http_code}' --config "$work/auth.curl" "$api/v1/auth/me")
[ "$status" = 401 ] || { printf 'revoked REST session returned HTTP %s\n' "$status" >&2; exit 1; }
"$cli" --output json auth status | jq -e '.authenticated == false' >/dev/null
printf 'live auth, secure storage, REST revocation, and SSE revocation passed\n'

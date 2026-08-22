#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "live Workspace qualification must run as root"
[ -n "${BLAZN_FENCING_TOKEN:-}" ] && [ -n "${BLAZN_CORRELATION_ID:-}" ] || die "live Workspace qualification must run through with-control-plane-lock.sh"
[ "$#" -eq 2 ] || die "usage: verify-live-workspace.sh CLI API_URL"
cli=$1
api=${2%/}
[ -x "$cli" ] || die "CLI is not executable"
case $api in https://*) ;; *) die "live Workspace API URL must use HTTPS" ;; esac
require_command curl
require_command jq

owner_login=${BLAZN_INITIAL_LOGIN:-}
owner_password=${BLAZN_SECRETS_ROOT:-/etc/blazn/control-plane/secrets}/initial-password
identity_root=${BLAZN_POC_IDENTITY_ROOT:-/var/lib/blazn/poc-identities/second}
second_profile=$identity_root/profile.json
second_password=$identity_root/password
for file in "$owner_password" "$second_profile" "$second_password"; do assert_regular_file_owned_mode "$file" 0 444; done
[ -n "$owner_login" ] || die "BLAZN_INITIAL_LOGIN is required"
second_login=$(jq -er '.login | select(type=="string")' "$second_profile")

work=$(mktemp -d /tmp/blazn-workspace-live.XXXXXX)
case $work in /tmp/blazn-workspace-live.*) ;; *) die "unsafe Workspace qualification path" ;; esac
login_pid=
cleanup() {
  [ -z "$login_pid" ] || kill "$login_pid" 2>/dev/null || true
  if [ -d "$work" ] && [ ! -L "$work" ]; then
    find "$work" -xdev -type f -delete
    find "$work" -xdev -depth -type d -empty -delete
  fi
}
trap cleanup EXIT HUP INT TERM
umask 077
mkdir "$work/owner-home" "$work/owner-data"
owner_home=$work/owner-home
owner_data=$work/owner-data
second_home=$identity_root/home
second_data=$identity_root/data
assert_directory_owned_mode "$second_home" 0 700
assert_directory_owned_mode "$second_data" 0 700

owner_cli() { HOME="$owner_home" XDG_DATA_HOME="$owner_data" BLAZN_API_URL="$api" "$cli" "$@"; }
second_cli() { HOME="$second_home" XDG_DATA_HOME="$second_data" BLAZN_API_URL="$api" "$cli" "$@"; }

login_identity() {
  which_identity=$1
  login=$2
  password_file=$3
  output=$work/$which_identity-login.out
  error=$work/$which_identity-login.err
  if [ "$which_identity" = owner ]; then owner_cli auth login --no-browser >"$output" 2>"$error" & else second_cli auth login --no-browser >"$output" 2>"$error" & fi
  login_pid=$!
  code=
  attempt=0
  while [ "$attempt" -lt 100 ]; do
    code=$(sed -n 's/.* enter code \([A-HJ-NP-Z2-9][A-HJ-NP-Z2-9-]*\)$/\1/p' "$output" | head -1)
    [ -z "$code" ] || break
    kill -0 "$login_pid" 2>/dev/null || break
    attempt=$((attempt + 1))
    sleep 0.1
  done
  [ -n "$code" ] || die "device code was not emitted for a qualification identity"
  password_value=$(tr -d '\r\n' <"$password_file")
  [ -n "$password_value" ] || die "qualification password file is empty"
  curl_config=$work/$which_identity-password.curl
  printf 'data-urlencode = "password=%s"\n' "$password_value" >"$curl_config"
  unset password_value
  curl --fail --silent --show-error --data-urlencode "user_code=$code" --data-urlencode "email=$login" --config "$curl_config" "$api/v1/auth/device/approve" >"$work/$which_identity-approval.html"
  wait "$login_pid"
  login_pid=
}

login_identity owner "$owner_login" "$owner_password"
login_identity second "$second_login" "$second_password"
owner_cli --output json auth status | jq -e '.authenticated==true' >/dev/null
second_cli --output json auth status | jq -e '.authenticated==true' >/dev/null

suffix=$(printf '%s' "$BLAZN_FENCING_TOKEN" | tail -c 8)
slug_a=poc-company-$suffix-a
slug_b=poc-company-$suffix-b
create_a=workspace-create-$suffix-a
create_b=workspace-create-$suffix-b
owner_cli --output json workspace create "POC Company A $suffix" --slug "$slug_a" --request-id "$create_a" >"$work/workspace-a.json"
owner_cli --output json workspace create "POC Company B $suffix" --slug "$slug_b" --request-id "$create_b" >"$work/workspace-b.json"
workspace_a=$(jq -er .workspace.id "$work/workspace-a.json")
workspace_b=$(jq -er .workspace.id "$work/workspace-b.json")
printf '%s\n' "$workspace_a" | "$SCRIPT_DIR/manage-poc-identity.sh" record-workspace >/dev/null
printf '%s\n' "$workspace_b" | "$SCRIPT_DIR/manage-poc-identity.sh" record-workspace >/dev/null

owner_cli --output json workspace invite "$workspace_a" --role member --expires-in 15m --request-id workspace-invite-$suffix \
  | jq -er .inviteToken \
  | second_cli workspace join --invite-stdin --request-id workspace-join-$suffix >"$work/join.out"
second_cli --output json workspace get "$workspace_a" | jq -e --arg id "$workspace_a" '.workspace.id==$id' >/dev/null

assert_denied() {
  label=$1
  shift
  if second_cli --output json "$@" >"$work/$label.json" 2>"$work/$label.err"; then die "cross-tenant API operation unexpectedly passed: $label"; fi
  jq -e '.code=="workspace_not_found" or .code=="forbidden"' "$work/$label.json" >/dev/null || die "cross-tenant API denial used an unexpected error contract: $label"
}
assert_denied hidden-get workspace get "$workspace_b"
assert_denied hidden-members workspace members "$workspace_b"
assert_denied hidden-invitations workspace invitations "$workspace_b"
version=$(jq -er .workspace.version "$work/workspace-a.json")
assert_denied member-edit workspace edit "$workspace_a" --name forbidden --expected-version "$version" --request-id member-edit-$suffix

owner_cli --output json workspace list | jq -e --arg a "$workspace_a" --arg b "$workspace_b" '([.items[].id] | contains([$a,$b]))' >/dev/null
second_cli --output json workspace list | jq -e --arg a "$workspace_a" --arg b "$workspace_b" '([.items[].id] | contains([$a]) and (contains([$b])|not))' >/dev/null
owner_cli auth logout >/dev/null
second_cli auth logout >/dev/null
printf 'two-user Workspace API isolation and role-denial qualification passed\n'

trap - EXIT HUP INT TERM
cleanup

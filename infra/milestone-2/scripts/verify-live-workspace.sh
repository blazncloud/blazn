#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "live Workspace qualification must run as root"
if [ -z "${BLAZN_FENCING_TOKEN:-}" ] || [ -z "${BLAZN_CORRELATION_ID:-}" ]; then die "live Workspace qualification must run through with-control-plane-lock.sh"; fi
[ "$#" -eq 2 ] || die "usage: verify-live-workspace.sh CLI API_URL"
cli=$1
api=${2%/}
[ -x "$cli" ] || die "CLI is not executable"
case $api in https://*) ;; *) die "live Workspace API URL must use HTTPS" ;; esac
require_command curl
require_command jq
require_command getent
require_command setpriv

owner_login=${BLAZN_INITIAL_LOGIN:-}
owner_password=${BLAZN_SECRETS_ROOT:-/etc/blazn/control-plane/secrets}/initial-password
identity_root=${BLAZN_POC_IDENTITY_ROOT:-/var/lib/blazn/poc-identities/second}
identity_receipt=${BLAZN_POC_IDENTITY_RECEIPT:-/var/lib/blazn/ownership/poc-second-identity.json}
cli_users_receipt=${BLAZN_POC_CLI_USERS_RECEIPT:-/var/lib/blazn/ownership/poc-cli-users.json}
second_profile=$identity_root/profile.json
second_password=$identity_root/password
for file in "$owner_password" "$second_profile" "$second_password"; do assert_regular_file_owned_mode "$file" 0 444; done
for file in "$identity_receipt" "$cli_users_receipt"; do assert_regular_file_owned_mode "$file" 0 600; done
[ -n "$owner_login" ] || die "BLAZN_INITIAL_LOGIN is required"
second_login=$(jq -er '.login | select(type=="string")' "$second_profile")

work=$(mktemp -d /tmp/blazn-workspace-live.XXXXXX)
case $work in /tmp/blazn-workspace-live.*) ;; *) die "unsafe Workspace qualification path" ;; esac
login_pid=
owner_home=
second_home=
cleanup() {
  [ -z "$login_pid" ] || kill "$login_pid" 2>/dev/null || true
  if [ -n "$owner_home" ]; then owner_cli auth logout >/dev/null 2>&1 || true; fi
  if [ -n "$second_home" ]; then second_cli auth logout >/dev/null 2>&1 || true; fi
  if [ -d "$work" ] && [ ! -L "$work" ]; then
    find "$work" -xdev -type f -delete
    find "$work" -xdev -depth -type d -empty -delete
  fi
}
trap cleanup EXIT HUP INT TERM
umask 077
owner_name=$(jq -er .owner.name "$cli_users_receipt"); owner_uid=$(jq -er .owner.uid "$cli_users_receipt"); owner_gid=$(jq -er .owner.gid "$cli_users_receipt"); owner_home=$(jq -er .owner.home "$cli_users_receipt")
second_name=$(jq -er .second.name "$cli_users_receipt"); second_uid=$(jq -er .second.uid "$cli_users_receipt"); second_gid=$(jq -er .second.gid "$cli_users_receipt"); second_home=$(jq -er .second.home "$cli_users_receipt")
[ "$owner_uid" != "$second_uid" ] || die "qualification CLI OS users share a UID"
for spec in "$owner_name:$owner_uid:$owner_gid:$owner_home" "$second_name:$second_uid:$second_gid:$second_home"; do
  name=${spec%%:*}; rest=${spec#*:}; uid=${rest%%:*}; rest=${rest#*:}; gid=${rest%%:*}; home=${rest#*:}
  passwd_record=$(getent passwd "$name") || die "qualification CLI OS user is absent"
  [ "$(printf '%s\n' "$passwd_record" | cut -d: -f3)" = "$uid" ] && [ "$(printf '%s\n' "$passwd_record" | cut -d: -f4)" = "$gid" ] && [ "$(printf '%s\n' "$passwd_record" | cut -d: -f6)" = "$home" ] || die "qualification CLI OS user differs from its receipt"
  [ "$(id -G "$name")" = "$gid" ] || die "qualification CLI OS user has supplementary groups"
  assert_directory_owned_mode "$home" "$uid" 700
done

owner_cli() { setpriv --reuid="$owner_uid" --regid="$owner_gid" --clear-groups --reset-env env -u DBUS_SESSION_BUS_ADDRESS -u XDG_RUNTIME_DIR -u XDG_DATA_HOME BLAZN_API_URL="$api" "$cli" "$@"; }
second_cli() { setpriv --reuid="$second_uid" --regid="$second_gid" --clear-groups --reset-env env -u DBUS_SESSION_BUS_ADDRESS -u XDG_RUNTIME_DIR -u XDG_DATA_HOME BLAZN_API_URL="$api" "$cli" "$@"; }

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
owner_cli --output json auth status >"$work/owner-status.json"
second_cli --output json auth status >"$work/second-status.json"
owner_user_id=$(jq -er '.user.id | select(type=="string")' "$work/owner-status.json")
second_user_id=$(jq -er '.user.id | select(type=="string")' "$work/second-status.json")
receipted_second_user_id=$(jq -er '.userId | select(type=="string")' "$identity_receipt")
[ "$second_user_id" = "$receipted_second_user_id" ] || die "second CLI authenticated as a user other than the receipted POC identity"
[ "$owner_user_id" != "$second_user_id" ] || die "owner and second CLI authenticated as the same user"

suffix=$(printf '%s' "$BLAZN_CORRELATION_ID" | sha256sum | awk '{print substr($1,1,12)}')
slug_a=poc-company-$suffix-a
slug_b=poc-company-$suffix-b
create_a=workspace-create-$suffix-a
create_b=workspace-create-$suffix-b
owner_cli --output json workspace create "POC Company A $suffix" --slug "$slug_a" --request-id "$create_a" >"$work/workspace-a.json"
workspace_a=$(jq -er .workspace.id "$work/workspace-a.json")
printf '%s\n' "$workspace_a" | "$SCRIPT_DIR/manage-poc-identity.sh" record-workspace >/dev/null
owner_cli --output json workspace create "POC Company B $suffix" --slug "$slug_b" --request-id "$create_b" >"$work/workspace-b.json"
workspace_b=$(jq -er .workspace.id "$work/workspace-b.json")
printf '%s\n' "$workspace_b" | "$SCRIPT_DIR/manage-poc-identity.sh" record-workspace >/dev/null

owner_cli --output json workspace invite "$workspace_a" --role member --expires-in 15m --request-id "workspace-invite-$suffix" \
  | jq -er .inviteToken \
  | second_cli workspace join --invite-stdin --request-id "workspace-join-$suffix" >"$work/join.out"
second_cli --output json workspace get "$workspace_a" | jq -e --arg id "$workspace_a" '.workspace.id==$id' >/dev/null

assert_denied() {
  label=$1
  shift
  if second_cli --output json "$@" >"$work/$label.json" 2>"$work/$label.err"; then die "cross-tenant API operation unexpectedly passed: $label"; fi
  jq -e '.error.code=="workspace_not_found" or .error.code=="forbidden"' "$work/$label.json" >/dev/null || die "cross-tenant API denial used an unexpected error contract: $label"
}
assert_denied hidden-get workspace get "$workspace_b"
assert_denied hidden-members workspace members "$workspace_b"
assert_denied hidden-invitations workspace invitations "$workspace_b"
version=$(jq -er .workspace.version "$work/workspace-a.json")
assert_denied member-edit workspace edit "$workspace_a" --name forbidden --expected-version "$version" --request-id "member-edit-$suffix"

owner_cli --output json workspace list | jq -e --arg a "$workspace_a" --arg b "$workspace_b" '([.items[].id] | contains([$a,$b]))' >/dev/null
second_cli --output json workspace list | jq -e --arg a "$workspace_a" --arg b "$workspace_b" '([.items[].id] | contains([$a]) and (contains([$b])|not))' >/dev/null
owner_cli auth logout >/dev/null
second_cli auth logout >/dev/null
printf 'two-user Workspace API isolation and role-denial qualification passed\n'

trap - EXIT HUP INT TERM
cleanup

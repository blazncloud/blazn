#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
# shellcheck disable=SC1091
. "$SCRIPT_DIR/common.sh"

[ "$(id -u)" -eq 0 ] || die "POC identity management must run as root"
if [ -z "${BLAZN_FENCING_TOKEN:-}" ] || [ -z "${BLAZN_CORRELATION_ID:-}" ]; then die "POC identity management must run through with-control-plane-lock.sh"; fi
[ "$#" -eq 1 ] || die "usage: manage-poc-identity.sh provision|record-workspace|cleanup"
action=$1
case $action in provision|record-workspace|cleanup) ;; *) die "unsupported POC identity action" ;; esac
require_command docker
require_command jq
require_command openssl
require_command sha256sum
export DOCKER_CONFIG="${BLAZN_DOCKER_CONFIG_ROOT:-/etc/blazn/docker-cli}"
ENV_FILE=${BLAZN_CONTROL_PLANE_ENV_FILE:-/etc/blazn/control-plane/control-plane.env}
assert_regular_file_owned_mode "$ENV_FILE" 0 600
load_control_api_image "$ROOT_DIR"

identity_root=${BLAZN_POC_IDENTITY_ROOT:-/var/lib/blazn/poc-identities/second}
receipt=${BLAZN_POC_IDENTITY_RECEIPT:-/var/lib/blazn/ownership/poc-second-identity.json}
active_release_receipt=${BLAZN_ACTIVE_RELEASE_RECEIPT:-/var/lib/blazn/ownership/active-release.json}
require_absolute_path BLAZN_POC_IDENTITY_ROOT "$identity_root"
require_absolute_path BLAZN_POC_IDENTITY_RECEIPT "$receipt"
assert_not_symlink_chain "$identity_root"
assert_not_symlink_chain "$receipt"
assert_regular_file_owned_mode "$active_release_receipt" 0 600
release_digest=$(jq -er '.releaseDigest | select(test("^sha256:[a-f0-9]{64}$"))' "$active_release_receipt")

profile=$identity_root/profile.json
password=$identity_root/password
user_id_file=$identity_root/user-id
workspaces=$identity_root/workspaces.json
result_tmp=
cleanup_temp() {
  [ -z "$result_tmp" ] || [ ! -e "$result_tmp" ] || unlink "$result_tmp"
}
trap cleanup_temp EXIT HUP INT TERM

validate_inputs() {
  assert_directory_owned_mode "$identity_root" 0 700
  for file in "$profile" "$password" "$user_id_file" "$workspaces"; do assert_regular_file_owned_mode "$file" 0 444; done
  jq -e 'keys == ["displayName","login"] and (.login|type=="string") and (.displayName|type=="string")' "$profile" >/dev/null || die "POC identity profile is invalid"
  password_value=$(sed -n '1p' "$password")
  [ "${#password_value}" -eq 48 ] || die "POC identity password has an unexpected length"
  case $password_value in *[!a-f0-9]*) die "POC identity password is invalid" ;; esac
  unset password_value
  grep -Eq '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$' "$user_id_file" || die "POC identity user ID is invalid"
  jq -e 'keys == ["workspaceIds"] and (.workspaceIds|type=="array") and ((.workspaceIds|unique|length) == (.workspaceIds|length))' "$workspaces" >/dev/null || die "POC identity workspace inventory is invalid"
}

compose_run() {
  service=$1
  docker compose -f "$ROOT_DIR/compose.yaml" --env-file "$ENV_FILE" --profile poc-identity run --rm -T "$service"
}

case $action in
  provision)
    "$SCRIPT_DIR/manage-poc-cli-users.sh" provision >/dev/null
    if [ -e "$receipt" ]; then
      assert_regular_file_owned_mode "$receipt" 0 600
      jq -e --arg releaseDigest "$release_digest" '.schemaVersion == "blazn.dev/poc-second-identity/v1" and .status == "active" and .releaseDigest == $releaseDigest' "$receipt" >/dev/null || die "existing POC identity receipt is not active for this release"
      validate_inputs
      jq -e --arg profile "sha256:$(sha256_file "$profile")" --arg password "sha256:$(sha256_file "$password")" --arg workspaces "sha256:$(sha256_file "$workspaces")" --arg userId "$(sed -n '1p' "$user_id_file")" \
        '.profileDigest==$profile and .passwordDigest==$password and .workspacesDigest==$workspaces and .userId==$userId' "$receipt" >/dev/null || die "POC identity files differ from their receipt"
      result_tmp=$identity_root/provision-result.tmp.$$
      compose_run poc-identity-provision >"$result_tmp"
      jq -e --arg userId "$(sed -n '1p' "$user_id_file")" '.userId==$userId and (.status=="created" or .status=="existing")' "$result_tmp" >/dev/null || die "POC identity retry returned an unexpected result"
      unlink "$result_tmp"
      result_tmp=
      printf 'POC second identity is already provisioned\n'
      exit 0
    fi
    umask 077
    if [ ! -e "$identity_root" ]; then mkdir -p -- "$identity_root"; fi
    assert_directory_owned_mode "$identity_root" 0 700
    mkdir -p -- "$(dirname -- "$receipt")"
    chmod 0700 "$(dirname -- "$receipt")"
    if [ ! -e "$profile" ]; then jq -cn --arg login "${BLAZN_POC_SECOND_LOGIN:-poc-second@blazn.invalid}" --arg displayName "${BLAZN_POC_SECOND_DISPLAY_NAME:-Blazn POC Second User}" '{login:$login,displayName:$displayName}' >"$profile"; fi
    if [ ! -e "$password" ]; then openssl rand -hex 24 >"$password"; fi
    if [ ! -e "$workspaces" ]; then jq -cn '{workspaceIds:[]}' >"$workspaces"; fi
    chmod 0444 "$profile" "$password" "$workspaces"
    for staged in "$profile" "$password" "$workspaces"; do assert_regular_file_owned_mode "$staged" 0 444; done
    result_tmp=$identity_root/provision-result.tmp.$$
    [ ! -e "$result_tmp" ] || die "POC identity result staging path exists"
    compose_run poc-identity-provision >"$result_tmp"
    user_id=$(jq -er '.userId | select(test("^[0-9a-f-]{36}$"))' "$result_tmp")
    jq -e '.status=="created" or .status=="existing"' "$result_tmp" >/dev/null || die "POC identity provision returned an unexpected result"
    if [ -e "$user_id_file" ]; then
      assert_regular_file_owned_mode "$user_id_file" 0 444
      [ "$(sed -n '1p' "$user_id_file")" = "$user_id" ] || die "partial POC identity user ID differs from database result"
    else
      printf '%s\n' "$user_id" >"$user_id_file"
      chmod 0444 "$user_id_file"
    fi
    unlink "$result_tmp"
    result_tmp=
    receipt_tmp=$receipt.tmp.$$
    jq -cn --arg status active --arg userId "$user_id" --arg profileDigest "sha256:$(sha256_file "$profile")" \
      --arg passwordDigest "sha256:$(sha256_file "$password")" --arg workspacesDigest "sha256:$(sha256_file "$workspaces")" \
      --arg releaseDigest "$release_digest" --arg createdAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
      --argjson fencingToken "$BLAZN_FENCING_TOKEN" --arg correlationId "$BLAZN_CORRELATION_ID" \
      '{schemaVersion:"blazn.dev/poc-second-identity/v1",status:$status,userId:$userId,profileDigest:$profileDigest,passwordDigest:$passwordDigest,workspacesDigest:$workspacesDigest,releaseDigest:$releaseDigest,createdAt:$createdAt,fencingToken:$fencingToken,correlationId:$correlationId}' >"$receipt_tmp"
    chmod 0600 "$receipt_tmp"
    mv -- "$receipt_tmp" "$receipt"
    validate_inputs
    printf 'POC second identity provisioned with an isolated credential home\n'
    ;;
  record-workspace)
    assert_regular_file_owned_mode "$receipt" 0 600
    validate_inputs
    IFS= read -r workspace_id
    case $workspace_id in ????????-????-????-????-????????????) ;; *) die "qualification workspace ID from stdin is invalid" ;; esac
    next=$identity_root/workspaces.next.$$
    jq --arg id "$workspace_id" '.workspaceIds = ((.workspaceIds + [$id]) | unique)' "$workspaces" >"$next"
    chmod 0444 "$next"
    mv -- "$next" "$workspaces"
    receipt_tmp=$receipt.tmp.$$
    jq --arg digest "sha256:$(sha256_file "$workspaces")" '.workspacesDigest=$digest' "$receipt" >"$receipt_tmp"
    chmod 0600 "$receipt_tmp"
    mv -- "$receipt_tmp" "$receipt"
    printf 'recorded one qualification workspace for exact cleanup\n'
    ;;
  cleanup)
    "$SCRIPT_DIR/manage-poc-cli-users.sh" cleanup >/dev/null
    assert_regular_file_owned_mode "$receipt" 0 600
    validate_inputs
    jq -e --arg profile "sha256:$(sha256_file "$profile")" --arg password "sha256:$(sha256_file "$password")" --arg workspaces "sha256:$(sha256_file "$workspaces")" \
      '.status=="active" and .profileDigest==$profile and .passwordDigest==$password and .workspacesDigest==$workspaces' "$receipt" >/dev/null || die "POC identity cleanup inputs differ from their receipt"
    result_tmp=$identity_root/cleanup-result.tmp.$$
    compose_run poc-identity-cleanup >"$result_tmp"
    jq -e --arg userId "$(sed -n '1p' "$user_id_file")" '.status=="cleaned" and .userId==$userId' "$result_tmp" >/dev/null || die "POC identity cleanup returned an unexpected result"
    cleanup_counts=$(jq -c '{workspaceCount,deviceCount,authorizationCount}' "$result_tmp")
    unlink "$result_tmp"
    result_tmp=
    for file in "$profile" "$password" "$user_id_file" "$workspaces"; do unlink "$file"; done
    rmdir "$identity_root"
    receipt_tmp=$receipt.tmp.$$
    jq --arg cleanedAt "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" --argjson cleanupCounts "$cleanup_counts" '.status="cleaned" | .cleanedAt=$cleanedAt | .cleanupCounts=$cleanupCounts' "$receipt" >"$receipt_tmp"
    chmod 0600 "$receipt_tmp"
    mv -- "$receipt_tmp" "$receipt"
    printf 'POC second identity, devices, sessions, and recorded qualification workspaces cleaned\n'
    ;;
esac
trap - EXIT HUP INT TERM

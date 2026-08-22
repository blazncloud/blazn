#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH='' cd -- "$TEST_DIR/../../.." && pwd)
MANAGE=$REPO_ROOT/infra/milestone-2/scripts/manage-poc-identity.sh
command -v sudo >/dev/null 2>&1 || { printf 'POC identity tests skipped: sudo unavailable\n'; exit 0; }
sudo -n true >/dev/null 2>&1 || { printf 'POC identity tests skipped: passwordless sudo unavailable\n'; exit 0; }

top=${TMPDIR:-/tmp}/blazn-poc-identity-test-$$
owner_os=bzpo$$
second_os=bzps$$
mkdir "$top" "$top/bin"
cleanup() {
  for account in "$owner_os" "$second_os"; do
    if record=$(getent passwd "$account" 2>/dev/null); then
      home=$(printf '%s\n' "$record" | cut -d: -f6)
      case $home in "$top"/*) ;; *) printf 'refusing cleanup of unexpected test account home\n' >&2; return 1 ;; esac
      sudo userdel "$account" >/dev/null 2>&1 || true
      if [ -d "$home" ] && [ ! -L "$home" ]; then sudo find "$home" -xdev -type f -delete; sudo find "$home" -xdev -depth -type d -empty -delete; fi
      if sudo getent group "$account" >/dev/null 2>&1; then sudo groupdel "$account" >/dev/null 2>&1 || true; fi
    fi
  done
  sudo find "$top" -xdev -type f -delete
  sudo find "$top" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM

cat >"$top/bin/docker" <<'EOF'
#!/bin/sh
set -eu
case "$1:$2" in
  image:inspect) printf '%s\n' "$FAKE_IMAGE_ID" ;;
  compose:-f)
    case "$*" in
      *"poc-identity-provision")
        printf '{"status":"existing","userId":"123e4567-e89b-42d3-a456-426614174099"}\n'
        [ "${FAKE_FAIL_AFTER_OUTPUT:-0}" -eq 0 ] || exit 44
        ;;
      *"poc-identity-cleanup") printf '{"status":"cleaned","userId":"123e4567-e89b-42d3-a456-426614174099","workspaceCount":1,"deviceCount":1,"authorizationCount":1}\n' ;;
      *"poc-identity-verify-cleanup") printf '{"status":"absent","userId":"123e4567-e89b-42d3-a456-426614174099"}\n' ;;
      *) printf 'unexpected Compose call: %s\n' "$*" >&2; exit 97 ;;
    esac
    ;;
  *) printf 'unexpected Docker call: %s\n' "$*" >&2; exit 98 ;;
esac
EOF
chmod 0755 "$top/bin/docker"

source_digest=sha256:$(
  # shellcheck disable=SC1091
  . "$REPO_ROOT/infra/milestone-2/scripts/common.sh"
  control_api_source_digest "$REPO_ROOT/infra/milestone-2"
)
image=blazn-control-api:source-${source_digest#sha256:}
image_id=sha256:$(printf '%064d' 4)

fixture=$top/fixture
mkdir "$fixture" "$fixture/ownership"
chmod 0700 "$fixture/ownership"
printf 'test env\n' >"$fixture/control-plane.env"
chmod 0600 "$fixture/control-plane.env"
jq -cn --arg sourceDigest "$source_digest" --arg image "$image" --arg imageId "$image_id" \
  '{schemaVersion:"blazn.dev/control-api-build/v1",sourceDigest:$sourceDigest,image:$image,imageId:$imageId,builtAt:"2026-08-22T00:00:00Z"}' >"$fixture/ownership/build.json"
jq -cn '{schemaVersion:"blazn.dev/active-release/v1",releaseDigest:("sha256:" + ("2" * 64))}' >"$fixture/ownership/active-release.json"
chmod 0600 "$fixture/ownership"/*.json
sudo chown -R 0:0 "$fixture/ownership" "$fixture/control-plane.env"

run_manage() {
  action=$1
  shift
  sudo env PATH="$top/bin:$PATH" FAKE_IMAGE_ID="$image_id" FAKE_FAIL_AFTER_OUTPUT="${FAKE_FAIL_AFTER_OUTPUT:-0}" \
    BLAZN_FENCING_TOKEN=12 BLAZN_CORRELATION_ID=poc-identity-test \
    BLAZN_CONTROL_PLANE_ENV_FILE="$fixture/control-plane.env" \
    BLAZN_CONTROL_API_BUILD_RECEIPT="$fixture/ownership/build.json" \
    BLAZN_ACTIVE_RELEASE_RECEIPT="$fixture/ownership/active-release.json" \
    BLAZN_POC_IDENTITY_ROOT="$fixture/identity" BLAZN_POC_IDENTITY_RECEIPT="$fixture/ownership/identity.json" \
    BLAZN_POC_IDENTITY_CLEANUP_INTENT="$fixture/ownership/identity-cleanup.json" \
    BLAZN_POC_IDENTITY_CLEANUP_RUNTIME="$fixture/ownership/identity-cleanup.runtime.json" \
    BLAZN_POC_CLI_USERS_ROOT="$fixture/cli-users" BLAZN_POC_CLI_USERS_RECEIPT="$fixture/ownership/cli-users.json" \
    BLAZN_POC_CLI_USERS_INTENT="$fixture/ownership/cli-users-intent.json" \
    BLAZN_POC_OWNER_OS_USER="$owner_os" BLAZN_POC_SECOND_OS_USER="$second_os" \
    BLAZN_POC_CLI_CLEANUP_FAILPOINT="${FAKE_POC_CLI_FAILPOINT:-}" \
    BLAZN_POC_IDENTITY_CLEANUP_FAILPOINT="${FAKE_POC_IDENTITY_FAILPOINT:-}" \
    "$MANAGE" "$action" "$@"
}

root_store_digest() {
  sudo sh -euc '
    path=/root/.local/share/blazn
    if [ ! -e "$path" ]; then printf "absent\n"; exit; fi
    [ -d "$path" ] && [ ! -L "$path" ]
    find "$path" -xdev -type f -print0 | sort -z | xargs -0 -r sha256sum | sha256sum | cut -d" " -f1
  '
}
root_before=$(root_store_digest)

if FAKE_FAIL_AFTER_OUTPUT=1 run_manage provision >"$fixture/fault.out" 2>"$fixture/fault.err"; then
  printf 'post-database provision fault unexpectedly passed\n' >&2
  exit 1
fi
[ -d "$fixture/identity" ] && [ ! -e "$fixture/ownership/identity.json" ]
run_manage provision >"$fixture/provision.out"
sudo jq -e '.status=="active" and .userId=="123e4567-e89b-42d3-a456-426614174099" and (.passwordDigest|test("^sha256:[a-f0-9]{64}$"))' "$fixture/ownership/identity.json" >/dev/null
sudo jq -e --arg owner "$owner_os" --arg second "$second_os" '.status=="active" and .owner.name==$owner and .second.name==$second and .owner.uid != .second.uid and .owner.home != .second.home' "$fixture/ownership/cli-users.json" >/dev/null
owner_uid=$(sudo jq -r .owner.uid "$fixture/ownership/cli-users.json"); owner_gid=$(sudo jq -r .owner.gid "$fixture/ownership/cli-users.json"); owner_home=$(sudo jq -r .owner.home "$fixture/ownership/cli-users.json")
second_uid=$(sudo jq -r .second.uid "$fixture/ownership/cli-users.json"); second_gid=$(sudo jq -r .second.gid "$fixture/ownership/cli-users.json"); second_home=$(sudo jq -r .second.home "$fixture/ownership/cli-users.json")
sudo setpriv --reuid="$owner_uid" --regid="$owner_gid" --clear-groups --reset-env sh -euc '[ "$HOME" = "$1" ]; mkdir -p "$HOME/.local/share/blazn/credentials"; printf owner >"$HOME/.local/share/blazn/credentials/probe"' sh "$owner_home"
sudo setpriv --reuid="$second_uid" --regid="$second_gid" --clear-groups --reset-env sh -euc '[ "$HOME" = "$1" ]; mkdir -p "$HOME/.local/share/blazn/credentials"; printf second >"$HOME/.local/share/blazn/credentials/probe"' sh "$second_home"
[ "$(sudo cat "$owner_home/.local/share/blazn/credentials/probe")" = owner ]
[ "$(sudo cat "$second_home/.local/share/blazn/credentials/probe")" = second ]
[ "$(sudo stat -c %u "$owner_home/.local/share/blazn/credentials/probe")" = "$owner_uid" ]
[ "$(sudo stat -c %u "$second_home/.local/share/blazn/credentials/probe")" = "$second_uid" ]
before=$(sudo sha256sum "$fixture/identity/password")
run_manage provision >"$fixture/retry.out"
after=$(sudo sha256sum "$fixture/identity/password")
[ "$before" = "$after" ] || { printf 'identity retry changed the password\n' >&2; exit 1; }
if grep -F 'poc-second@blazn.invalid' "$fixture"/*.out >/dev/null || grep -E '[a-f0-9]{48}' "$fixture"/*.out >/dev/null; then
  printf 'POC identity command output leaked profile or credential material\n' >&2
  exit 1
fi

printf '123e4567-e89b-42d3-a456-426614174088\n' | run_manage record-workspace >"$fixture/record.out"
sudo jq -e '.workspaceIds==["123e4567-e89b-42d3-a456-426614174088"]' "$fixture/identity/workspaces.json" >/dev/null
for failpoint in after-second-user after-second-group after-second-home after-owner-user after-owner-group after-owner-home after-receipt; do
  if FAKE_POC_CLI_FAILPOINT=$failpoint run_manage cleanup >"$fixture/$failpoint.out" 2>"$fixture/$failpoint.err"; then
    printf 'POC CLI cleanup failpoint unexpectedly passed: %s\n' "$failpoint" >&2
    exit 1
  fi
  grep -F 'injected POC CLI cleanup failure' "$fixture/$failpoint.err" >/dev/null
done
for failpoint in after-db after-files after-receipt; do
  if FAKE_POC_IDENTITY_FAILPOINT=$failpoint run_manage cleanup >"$fixture/identity-$failpoint.out" 2>"$fixture/identity-$failpoint.err"; then
    printf 'POC identity cleanup failpoint unexpectedly passed: %s\n' "$failpoint" >&2
    exit 1
  fi
  grep -F 'injected POC identity cleanup failure' "$fixture/identity-$failpoint.err" >/dev/null
  [ -e "$fixture/ownership/identity-cleanup.runtime.json" ]
  [ "$(sudo stat -c %a "$fixture/ownership/identity-cleanup.runtime.json")" = 444 ]
done
run_manage cleanup >"$fixture/cleanup.out"
[ ! -e "$fixture/identity" ]
[ ! -e "$fixture/cli-users" ]
[ ! -e "$fixture/ownership/identity-cleanup.runtime.json" ]
getent passwd "$owner_os" >/dev/null 2>&1 && exit 1
getent passwd "$second_os" >/dev/null 2>&1 && exit 1
sudo jq -e '.status=="cleaned" and .cleanupCounts.workspaceCount==1' "$fixture/ownership/identity.json" >/dev/null
sudo sh -euc 'printf "stale-runtime-copy\n" >"$1"; chown 0:0 "$1"; chmod 0444 "$1"' sh "$fixture/ownership/identity-cleanup.runtime.json"
run_manage cleanup >"$fixture/cleaned-retry.out"
[ ! -e "$fixture/ownership/identity-cleanup.runtime.json" ]
[ "$(root_store_digest)" = "$root_before" ] || { printf 'existing root Blazn state changed during POC CLI qualification test\n' >&2; exit 1; }

trap - EXIT HUP INT TERM
cleanup
printf 'POC identity retry, receipt, isolated-home, inventory, and cleanup tests passed\n'

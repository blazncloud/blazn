#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH='' cd -- "$TEST_DIR/../../.." && pwd)
MANAGE=$REPO_ROOT/infra/milestone-2/scripts/manage-poc-identity.sh
command -v sudo >/dev/null 2>&1 || { printf 'POC identity tests skipped: sudo unavailable\n'; exit 0; }
sudo -n true >/dev/null 2>&1 || { printf 'POC identity tests skipped: passwordless sudo unavailable\n'; exit 0; }

top=${TMPDIR:-/tmp}/blazn-poc-identity-test-$$
mkdir "$top" "$top/bin"
cleanup() {
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
      *"poc-identity-cleanup") printf '{"status":"cleaned","userId":"123e4567-e89b-42d3-a456-426614174099","workspaceCount":1,"deviceCount":1,"authorizationCount":1,"rateLimitCount":1}\n' ;;
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
    "$MANAGE" "$action" "$@"
}

if FAKE_FAIL_AFTER_OUTPUT=1 run_manage provision >"$fixture/fault.out" 2>"$fixture/fault.err"; then
  printf 'post-database provision fault unexpectedly passed\n' >&2
  exit 1
fi
[ -d "$fixture/identity" ] && [ ! -e "$fixture/ownership/identity.json" ]
run_manage provision >"$fixture/provision.out"
sudo jq -e '.status=="active" and .userId=="123e4567-e89b-42d3-a456-426614174099" and (.passwordDigest|test("^sha256:[a-f0-9]{64}$"))' "$fixture/ownership/identity.json" >/dev/null
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
sudo sh -c 'printf credential >"$1/data/session.v1"' sh "$fixture/identity"
run_manage cleanup >"$fixture/cleanup.out"
[ ! -e "$fixture/identity" ]
sudo jq -e '.status=="cleaned" and .cleanupCounts.workspaceCount==1' "$fixture/ownership/identity.json" >/dev/null

trap - EXIT HUP INT TERM
cleanup
printf 'POC identity retry, receipt, isolated-home, inventory, and cleanup tests passed\n'

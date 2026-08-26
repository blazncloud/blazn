#!/bin/sh
set -eu

TEST_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH='' cd -- "$TEST_DIR/.." && pwd)
command -v sudo >/dev/null 2>&1 || { printf 'identity overlay tests skipped: sudo unavailable\n'; exit 0; }
sudo -n true >/dev/null 2>&1 || { printf 'identity overlay tests skipped: passwordless sudo unavailable\n'; exit 0; }

top=${TMPDIR:-/tmp}/blazn-identity-overlay-test-$$
mkdir -p "$top/infra"
cleanup() {
  sudo find "$top" -xdev -type f -delete
  sudo find "$top" -xdev -depth -type d -empty -delete
}
trap cleanup EXIT HUP INT TERM
printf 'services: {}\n' >"$top/infra/compose.yaml"
printf 'services: {}\n' >"$top/infra/compose.identity.yaml"
printf 'PUBLIC_URL=https://blazn.benpelo.com\n' >"$top/control-plane.env"
{
  printf 'ZITADEL_ISSUER_URL=https://auth.blazn.benpelo.com\n'
  printf 'ZITADEL_REVIEWED_RELEASE=v4.17.1\n'
  printf 'ZITADEL_REVIEWED_ASSURANCE_POLICY_DIGEST=sha256:%064d\n' 0
  printf 'ZITADEL_REVIEWED_ACR_POLICY=zitadel-v4.17.1-empty\n'
  printf 'ZITADEL_REVIEWED_MFA_AMR_SETS=pwd+mfa+otp;user+mfa\n'
} >"$top/identity.env"
chmod 0600 "$top/identity.env"
sudo chown 0:0 "$top/identity.env"

docker() { printf '%s\n' "$*" >"$top/docker.args"; }
# shellcheck disable=SC1091
. "$ROOT_DIR/scripts/common.sh"

BLAZN_IDENTITY_ENABLED=false
export BLAZN_IDENTITY_ENABLED
control_plane_compose "$top/infra" "$top/control-plane.env" ps api
grep -Fx "compose -f $top/infra/compose.yaml --env-file $top/control-plane.env ps api" "$top/docker.args" >/dev/null

BLAZN_IDENTITY_ENABLED=true
BLAZN_IDENTITY_ENV_FILE=$top/identity.env
export BLAZN_IDENTITY_ENABLED BLAZN_IDENTITY_ENV_FILE
control_plane_compose "$top/infra" "$top/control-plane.env" ps api
grep -Fx "compose -f $top/infra/compose.yaml -f $top/infra/compose.identity.yaml --env-file $top/control-plane.env --env-file $top/identity.env ps api" "$top/docker.args" >/dev/null
sudo chmod 0644 "$top/identity.env"
validate_identity_policy_fields "$top/identity.env"

sed 's/^ZITADEL_REVIEWED_ASSURANCE_POLICY_DIGEST=.*/ZITADEL_REVIEWED_ASSURANCE_POLICY_DIGEST=invalid/' "$top/identity.env" >"$top/invalid-policy.env"
if (validate_identity_policy_fields "$top/invalid-policy.env") >"$top/policy.out" 2>"$top/policy.err"; then
  printf 'invalid identity assurance policy unexpectedly passed\n' >&2
  exit 1
fi
grep -F 'reviewed ZITADEL assurance policy digest is invalid' "$top/policy.err" >/dev/null

sed 's/^ZITADEL_REVIEWED_ACR_POLICY=.*/ZITADEL_REVIEWED_ACR_POLICY=accept-any-empty-acr/' "$top/identity.env" >"$top/invalid-acr.env"
if (validate_identity_policy_fields "$top/invalid-acr.env") >"$top/acr.out" 2>"$top/acr.err"; then
  printf 'unreviewed identity ACR policy unexpectedly passed\n' >&2
  exit 1
fi
grep -F 'reviewed ZITADEL ACR policy must require v4.17.1 empty ACR' "$top/acr.err" >/dev/null

sed 's/^ZITADEL_REVIEWED_MFA_AMR_SETS=.*/ZITADEL_REVIEWED_MFA_AMR_SETS=pwd+otp;user/' "$top/identity.env" >"$top/invalid-amr.env"
if (validate_identity_policy_fields "$top/invalid-amr.env") >"$top/amr.out" 2>"$top/amr.err"; then
  printf 'weakened identity AMR policy unexpectedly passed\n' >&2
  exit 1
fi
grep -F 'reviewed ZITADEL MFA AMR sets must match v4.17.1' "$top/amr.err" >/dev/null

sudo mkdir -m 700 "$top/source-secrets" "$top/runtime-parent"
sudo install -o root -g root -m 0600 /dev/null "$top/source-secrets/zitadel-client-secret"
sudo install -o root -g root -m 0600 /dev/null "$top/source-secrets/oidc-cookie-key"
printf 'client-secret-value\n' | sudo tee "$top/source-secrets/zitadel-client-secret" >/dev/null
printf '0123456789012345678901234567890123456789012' | sudo tee "$top/source-secrets/oidc-cookie-key" >/dev/null
sudo sh -c ". '$ROOT_DIR/scripts/common.sh'; publish_identity_runtime_secrets '$top/source-secrets' '$top/runtime-parent/runtime-secrets'"
sudo sh -c ". '$ROOT_DIR/scripts/common.sh'; validate_identity_runtime_secrets '$top/source-secrets' '$top/runtime-parent/runtime-secrets'"
[ "$(sudo stat -c '%u:%a' "$top/runtime-parent/runtime-secrets/zitadel-client-secret")" = 0:444 ]
[ "$(sudo stat -c '%u:%a' "$top/runtime-parent/runtime-secrets/oidc-cookie-key")" = 0:444 ]
sudo sh -c "printf drift > '$top/runtime-parent/runtime-secrets/oidc-cookie-key'; chmod 0444 '$top/runtime-parent/runtime-secrets/oidc-cookie-key'"
# The redirected diagnostics are intentionally owned by the invoking test user.
# shellcheck disable=SC2024
if sudo sh -c ". '$ROOT_DIR/scripts/common.sh'; validate_identity_runtime_secrets '$top/source-secrets' '$top/runtime-parent/runtime-secrets'" >"$top/runtime.out" 2>"$top/runtime.err"; then
  printf 'drifted identity runtime secret unexpectedly passed\n' >&2
  exit 1
fi
grep -F 'published identity runtime secret differs' "$top/runtime.err" >/dev/null

sudo mkdir -m 700 "$top/node-source-secrets"
sudo install -o root -g root -m 0400 /dev/null "$top/node-source-secrets/enrollment-hmac-v1"
head -c 32 /dev/zero | sudo tee "$top/node-source-secrets/enrollment-hmac-v1" >/dev/null
sudo sh -c ". '$ROOT_DIR/scripts/common.sh'; publish_node_enrollment_runtime_secret '$top/node-source-secrets' '$top/runtime-parent/runtime-secrets'"
[ "$(sudo stat -c '%u:%a:%s' "$top/runtime-parent/runtime-secrets/node-enrollment-hmac-v1")" = 0:444:32 ]
sudo cmp -s "$top/node-source-secrets/enrollment-hmac-v1" "$top/runtime-parent/runtime-secrets/node-enrollment-hmac-v1"

BLAZN_IDENTITY_ENABLED=invalid
export BLAZN_IDENTITY_ENABLED
if (control_plane_compose "$top/infra" "$top/control-plane.env" ps api) >"$top/out" 2>"$top/err"; then
  printf 'invalid identity enablement unexpectedly passed\n' >&2
  exit 1
fi
grep -F 'BLAZN_IDENTITY_ENABLED must be true or false' "$top/err" >/dev/null

trap - EXIT HUP INT TERM
cleanup
printf 'identity Compose overlay selection tests passed\n'

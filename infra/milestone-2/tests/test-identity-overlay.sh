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
  printf 'ZITADEL_REVIEWED_ACR_VALUES=urn:zitadel:iam:org:project:roles,urn:blazn:mfa\n'
  printf 'ZITADEL_REVIEWED_MFA_AMR_SETS=pwd+otp;pwd+webauthn\n'
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

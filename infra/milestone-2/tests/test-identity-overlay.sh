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
  printf 'BLAZN_IDENTITY_SECRETS_ROOT=/etc/blazn/identity/secrets\n'
  printf 'ZITADEL_ISSUER_URL=https://auth.blazn.benpelo.com\n'
  printf 'ZITADEL_CLIENT_ID=123456789\n'
  printf 'ZITADEL_REQUIRE_MFA=true\n'
  printf 'ZITADEL_REVIEWED_RELEASE=v4.17.1\n'
  printf 'ZITADEL_REVIEWED_ASSURANCE_POLICY_DIGEST=sha256:%064d\n' 0
  printf 'ZITADEL_REVIEWED_ACR_VALUES=urn:zitadel:iam:org:project:roles,urn:blazn:mfa\n'
  printf 'ZITADEL_REVIEWED_MFA_AMR_SETS=pwd+otp;pwd+webauthn\n'
} >"$top/identity.env"
cp "$top/identity.env" "$top/identity.original"
chmod 0600 "$top/identity.env"

docker() {
  printf '%s\n' "$*" >"$top/docker.args"
  printf '%s\n' "${ZITADEL_ISSUER_URL-unset}" >"$top/docker.environment"
  previous=
  for argument in "$@"; do
    if [ "$previous" = --env-file ] && [ "$argument" != "$top/control-plane.env" ]; then
      cp "$argument" "$top/compose-identity.snapshot"
    fi
    previous=$argument
  done
  if [ "${mutate_identity_during_compose:-false}" = true ]; then
    sed -i 's/ZITADEL_REVIEWED_RELEASE=v4.17.1/ZITADEL_REVIEWED_RELEASE=v4.17.2/' "$top/identity.env"
  fi
}
# shellcheck disable=SC1091
. "$ROOT_DIR/scripts/common.sh"
validate_identity_overlay() {
  identity_env=${BLAZN_IDENTITY_ENV_FILE:-/etc/blazn/identity/control-api.env}
  validate_identity_environment_values "$identity_env"
}
sha256_file() { printf '%064d\n' 0; }

BLAZN_IDENTITY_ENABLED=false
export BLAZN_IDENTITY_ENABLED
control_plane_compose "$top/infra" "$top/control-plane.env" ps api
grep -Fx "compose -f $top/infra/compose.yaml --env-file $top/control-plane.env ps api" "$top/docker.args" >/dev/null

BLAZN_IDENTITY_ENABLED=true
BLAZN_IDENTITY_ENV_FILE=$top/identity.env
export BLAZN_IDENTITY_ENABLED BLAZN_IDENTITY_ENV_FILE
ZITADEL_ISSUER_URL=https://attacker.invalid
export ZITADEL_ISSUER_URL
control_plane_compose "$top/infra" "$top/control-plane.env" ps api
grep -E "^compose -f $top/infra/compose.yaml -f $top/infra/compose.identity.yaml --env-file $top/control-plane.env --env-file .*/blazn-identity-compose\.[A-Za-z0-9]+ ps api$" "$top/docker.args" >/dev/null
grep -Fx unset "$top/docker.environment" >/dev/null
cmp -s "$top/identity.env" "$top/compose-identity.snapshot" || {
  printf 'Compose identity snapshot differs from the validated canonical environment\n' >&2
  exit 1
}

mutate_identity_during_compose=true
export mutate_identity_during_compose
if control_plane_compose "$top/infra" "$top/control-plane.env" ps api; then
  printf 'identity mutation during Compose unexpectedly passed\n' >&2
  exit 1
fi
unset mutate_identity_during_compose
cp "$top/identity.original" "$top/identity.env"
chmod 0600 "$top/identity.env"

BLAZN_IDENTITY_ENABLED=false
export BLAZN_IDENTITY_ENABLED
disabled_digest=$(control_plane_config_digest "$ROOT_DIR")
BLAZN_IDENTITY_ENABLED=true
export BLAZN_IDENTITY_ENABLED
enabled_digest=$(control_plane_config_digest "$ROOT_DIR")
[ "$disabled_digest" != "$enabled_digest" ] || {
  printf 'enabled and disabled identity configuration digests are equal\n' >&2
  exit 1
}

sed 's/^ZITADEL_REVIEWED_ASSURANCE_POLICY_DIGEST=.*/ZITADEL_REVIEWED_ASSURANCE_POLICY_DIGEST=invalid/' "$top/identity.env" >"$top/invalid-policy.env"
if (validate_identity_policy_fields "$top/invalid-policy.env") >"$top/policy.out" 2>"$top/policy.err"; then
  printf 'invalid identity assurance policy unexpectedly passed\n' >&2
  exit 1
fi
grep -F 'reviewed ZITADEL assurance policy digest is invalid' "$top/policy.err" >/dev/null

cp "$top/identity.env" "$top/unknown.env"
printf 'POSTGRES_IMAGE=unreviewed\n' >>"$top/unknown.env"
if (validate_identity_environment_values "$top/unknown.env") >"$top/unknown.out" 2>"$top/unknown.err"; then
  printf 'unreviewed identity environment key unexpectedly passed\n' >&2
  exit 1
fi
grep -F 'unreviewed key' "$top/unknown.err" >/dev/null

cp "$top/identity.env" "$top/duplicate.env"
printf 'ZITADEL_REVIEWED_RELEASE=v4.17.1\n' >>"$top/duplicate.env"
if (validate_identity_environment_values "$top/duplicate.env") >"$top/duplicate.out" 2>"$top/duplicate.err"; then
  printf 'duplicate identity environment key unexpectedly passed\n' >&2
  exit 1
fi
grep -F 'must occur exactly once' "$top/duplicate.err" >/dev/null

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

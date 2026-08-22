#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
compose=$SCRIPT_DIR/compose.yaml
control_overlay=$SCRIPT_DIR/../milestone-2/compose.identity.yaml

for expected in \
  'image: ${ZITADEL_TRAEFIK_IMAGE:?set ZITADEL_TRAEFIK_IMAGE to a reviewed immutable digest}' \
  'image: ${ZITADEL_IMAGE:?set ZITADEL_IMAGE to a reviewed immutable digest}' \
  '--masterkeyFile=/run/secrets/zitadel_masterkey' \
  'ZITADEL_DEFAULTINSTANCE_FEATURES_LOGINV2_REQUIRED: "true"' \
  'ZITADEL_SYSTEMDEFAULTS_MULTIFACTORS_OTP_ISSUER: Blazn' \
  'ZITADEL_SERVICE_USER_TOKEN_FILE: /zitadel/bootstrap/login-client.pat' \
  '127.0.0.1}:${ZITADEL_PROXY_PORT:-58081}:8080' \
  'driver: bridge'; do
  grep -F -- "$expected" "$compose" >/dev/null || {
    printf 'identity compose contract is missing: %s\n' "$expected" >&2
    exit 1
  }
done

grep -F 'url: h2c://zitadel-api:8080' "$SCRIPT_DIR/traefik-routes.yaml" >/dev/null
grep -F 'url: http://zitadel-login:3000' "$SCRIPT_DIR/traefik-routes.yaml" >/dev/null
if grep -F '/var/run/docker.sock' "$compose" >/dev/null; then
  printf 'identity proxy receives the Docker socket\n' >&2
  exit 1
fi

if grep -E 'Password: [^R$]|MASTERKEY=' "$compose" "$SCRIPT_DIR"/*.example.yaml >/dev/null; then
  printf 'identity templates contain a literal credential\n' >&2
  exit 1
fi

grep -F 'PasswordChangeRequired: true' "$SCRIPT_DIR/zitadel-steps.example.yaml" >/dev/null
grep -F 'ZITADEL_ISSUER_URL' "$SCRIPT_DIR/../../services/control-api/src/config.ts" >/dev/null
grep -F 'ZITADEL_CLIENT_SECRET_FILE: /run/secrets/zitadel_client_secret' "$control_overlay" >/dev/null
grep -F 'OIDC_COOKIE_KEY_FILE: /run/secrets/oidc_cookie_key' "$control_overlay" >/dev/null
if grep -F 'ZITADEL_CLIENT_SECRET_FILE' "$SCRIPT_DIR/../milestone-2/compose.yaml" >/dev/null; then
  printf 'unqualified identity integration is enabled in the base control-plane compose file\n' >&2
  exit 1
fi
printf 'identity static contract: ok\n'

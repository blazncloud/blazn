#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
compose=$SCRIPT_DIR/compose.yaml
control_overlay=$SCRIPT_DIR/../milestone-2/compose.identity.yaml

for expected in \
  'image: ${ZITADEL_TRAEFIK_IMAGE:?set ZITADEL_TRAEFIK_IMAGE to a reviewed immutable digest}' \
  'image: ${ZITADEL_POSTGRES_IMAGE:?set ZITADEL_POSTGRES_IMAGE to a reviewed immutable digest}' \
  'image: ${ZITADEL_IMAGE:?set ZITADEL_IMAGE to a reviewed immutable digest}' \
	'image: ${ZITADEL_LOGIN_IMAGE:?set ZITADEL_LOGIN_IMAGE to a reviewed immutable digest}' \
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
for script in generate-secrets.sh validate-environment.sh backup.sh restore.sh test-disposable.sh test-secret-generation.sh; do
  sh -n "$SCRIPT_DIR/$script"
done
for required in \
  'secret generation must run as root' \
  "stat -c '%u:%a:%h'" \
  'mktemp "$secrets_root/.secret.tmp.XXXXXX"' \
  'sync -f'; do
  grep -F -- "$required" "$SCRIPT_DIR/generate-secrets.sh" >/dev/null || { printf 'secret generator safety contract missing: %s\n' "$required" >&2; exit 1; }
done
grep -F 'method="post" action="/v1/auth/oidc/start"' "$SCRIPT_DIR/../../services/control-api/src/auth-page.ts" >/dev/null
if grep -F 'method === "GET" && url.pathname === "/v1/auth/oidc/start"' "$SCRIPT_DIR/../../services/control-api/src/server.ts" >/dev/null; then
  printf 'OIDC start remains drive-by GET reachable\n' >&2; exit 1
fi
grep -F 'unsealActivationConfirmation' "$SCRIPT_DIR/../../services/control-api/src/server.ts" >/dev/null
grep -F 'ZITADEL_REVIEWED_ACR_VALUES' "$control_overlay" >/dev/null
grep -F 'acceptedAmrSets' "$SCRIPT_DIR/../../services/control-api/src/oidc.ts" >/dev/null
grep -F 'identityProvider: oidcClient ? "ok" : "disabled"' "$SCRIPT_DIR/../../services/control-api/src/server.ts" >/dev/null
grep -F '@sha256:REPLACE_64_HEX' "$SCRIPT_DIR/env.example" >/dev/null
for required in postgres.sql secrets.tar zitadel-bootstrap.tar SHA256SUMS; do
  grep -F "$required" "$SCRIPT_DIR/backup.sh" "$SCRIPT_DIR/restore.sh" >/dev/null || { printf 'backup/restore contract missing: %s\n' "$required" >&2; exit 1; }
done
node "$SCRIPT_DIR/verify-qualification.mjs" "$SCRIPT_DIR/qualification-receipt.test.json" >/dev/null
printf 'identity static contract: ok\n'

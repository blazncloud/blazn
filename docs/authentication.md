# Blazn authentication

Blazn uses a dedicated, self-hosted ZITADEL instance as its identity authority.
The identity database, encryption master key, administrator account, login
client, sessions, users, MFA policy, and social-provider configuration are not
shared with Frontro or any hosted identity tenant.

The initial integration is deliberately isolated. Existing provisioned Blazn
accounts continue to work while ZITADEL identities are enabled and tested.

## Request flow

1. `blazn auth login` creates a device-bound authorization in the Blazn API.
2. The CLI opens the branded `/activate` page and displays the same one-time
   code shown in the browser.
3. The browser starts Authorization Code flow with PKCE against the self-hosted
   ZITADEL issuer.
4. ZITADEL performs registration or login, email verification, social
   federation, and the configured MFA challenge.
5. The Blazn callback verifies the issuer, audience, nonce, signature, verified
   email, and MFA authentication-method claim before approving the device.
6. The CLI proves possession of its Ed25519 private key and receives a
   Blazn-scoped, revocable device session.

ZITADEL credentials and tokens never enter the CLI. The control API accepts a
ZITADEL identity only for the pending one-time device authorization that
started the browser transaction.

## Identity stack

The isolated stack is in `infra/identity`:

- PostgreSQL holds only ZITADEL state.
- `zitadel-api` exposes the identity and standards APIs.
- `zitadel-login` is the self-hosted Next.js login application.
- Traefik provides the required h2c connection to the API without access to the
  Docker socket.
- A private Docker volume transfers the generated login-client credential from
  ZITADEL to the login application.
- Only the Traefik loopback port `58081` is published by default.

Copy `infra/identity/env.example` to an operator-owned environment file, replace
all image tags with reviewed immutable digests, and generate root-owned secrets:

```sh
sudo install -d -m 0700 /etc/blazn/identity/secrets
sudo ./infra/identity/generate-secrets.sh \
  /etc/blazn/identity/secrets admin@your-company.example
```

The generated ZITADEL master key is not rotatable in place. Back it up before
first start. Rotate the initial administrator password on first login, then
remove `initial-admin-password` after recording the completed bootstrap.

Start the isolated stack only after its public origin and reverse-proxy routing
are ready:

```sh
docker compose --env-file /etc/blazn/identity/env \
  -f infra/identity/compose.yaml up -d --wait
```

Point the identity hostname's TLS tunnel at `http://127.0.0.1:58081`. The
file-configured internal Traefik proxy routes `/ui/v2/login/` to the login app
and every other path to the ZITADEL API over h2c. It does not receive the
Docker socket.

The external hostname, port, and secure flag must exactly match the public
issuer. A mismatch causes ZITADEL `Instance not found` failures.

## ZITADEL application

Create a confidential Web application named `blazn-control-api` with:

- Authorization Code flow;
- PKCE required;
- `client_secret_post` token endpoint authentication;
- redirect URI `https://<blazn-api>/v1/auth/oidc/callback`;
- post-logout URI `https://<blazn-api>/activate`;
- ID tokens signed with RS256.

Install the client secret and a separately generated 32-byte base64url cookie
encryption key as root-owned files outside the repository:

```text
/etc/blazn/identity/secrets/zitadel-client-secret
/etc/blazn/identity/secrets/oidc-cookie-key
```

Configure the control API with:

```text
ZITADEL_ISSUER_URL=https://<identity-hostname>
ZITADEL_CLIENT_ID=<blazn-control-api-client-id>
ZITADEL_CLIENT_SECRET_FILE=/run/secrets/zitadel_client_secret
ZITADEL_REQUIRE_MFA=true
OIDC_COOKIE_KEY_FILE=/run/secrets/oidc_cookie_key
```

These values and secrets are intentionally absent from the base control-plane
Compose file. After the qualification gate passes, enable the integration with
the additive overlay so an ordinary control-plane restart cannot accidentally
activate an unqualified identity provider:

```sh
docker compose \
  --env-file /etc/blazn/control-plane.env \
  --env-file /etc/blazn/identity/control-api.env \
  -f infra/milestone-2/compose.yaml \
  -f infra/milestone-2/compose.identity.yaml \
  up -d --wait
```

## Registration, social identity, and MFA policy

Enable self-registration and verified email. Configure Google, GitHub, and
Apple as instance-level external identity providers, but do not enable automatic
linking by email. A matching verified email is not sufficient proof that a new
social identity owns an existing Blazn account.

Require MFA for Blazn. Prefer passkeys/WebAuthn, allow TOTP as the recovery-
compatible fallback, and require recovery setup before production access. SMS
is not a primary factor.

The callback intentionally fails closed when the identity token lacks a
verified email or an accepted MFA `amr` value. Validate the exact `amr` values
emitted by the reviewed ZITADEL release during qualification before enabling
production traffic.

## Qualification gate

Do not replace the current live authentication route until all of these pass:

1. new email registration and email verification;
2. existing email/password login;
3. Google, GitHub, and Apple first login and repeat login;
4. passkey enrollment/login and TOTP enrollment/login;
5. recovery-code use and replay rejection;
6. CLI device approval, denial, expiration, and polling throttling;
7. logout, device revocation, access expiry, and refresh rotation;
8. duplicate-email and social-account-linking takeover tests;
9. database backup/restore and ZITADEL master-key recovery;
10. exact deployed-image digest, TLS, h2c proxy, and public issuer validation.

ZITADEL is AGPL-3.0 licensed. Running the unmodified upstream images is isolated
from Blazn source, but any ZITADEL modifications must receive an explicit
license/compliance review and corresponding-source plan before deployment.

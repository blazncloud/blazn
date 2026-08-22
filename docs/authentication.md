# Blazn authentication

Blazn uses its own isolated Auth0 tenant as an OpenID Connect provider. It does
not share the Frontro tenant, applications, sessions, users, or MFA policy.

## Application

Create one **Regular Web Application** with:

- Callback URL: `https://blazn.benpelo.com/v1/auth/oidc/callback`
- Login URL: `https://blazn.benpelo.com/activate`
- Allowed web origin: `https://blazn.benpelo.com`
- Authorization Code flow with PKCE
- RS256 ID tokens

Set these non-secret deployment values:

```text
AUTH0_ISSUER_URL=https://<isolated-tenant>.auth0.com
AUTH0_CLIENT_ID=<regular-web-application-client-id>
AUTH0_CONNECTIONS=google-oauth2,github,apple
AUTH0_REQUIRE_MFA=true
```

Install the application client secret and a freshly generated 32-byte
base64url cookie-encryption key as root-owned files outside the repository:

```text
/etc/blazn/identity/secrets/auth0-client-secret
/etc/blazn/identity/secrets/oidc-cookie-key
```

The service receives them only through Docker secret mounts. They must never be
placed in environment values, images, browser HTML, logs, receipts, or source.

## Connections and MFA

Enable a dedicated Auth0 database connection plus the reviewed Google, GitHub,
and Apple social connections only for the Blazn application. Require MFA for
the application using phishing-resistant WebAuthn/passkeys when available and
TOTP as the recovery-compatible fallback. Generate and test recovery codes
before production use. SMS is not the primary factor.

Auth0 Universal Login owns email verification, password reset, social consent,
MFA enrollment, MFA challenge, and account recovery. Blazn accepts an identity
only when the ID token:

- is signed with RS256 by the configured issuer;
- has the exact Blazn client audience and callback nonce;
- contains a verified email;
- contains an MFA authentication method when MFA is required.

Blazn does not automatically link a new provider identity to a legacy account
with the same email. That operation requires a separate authenticated linking
flow so a verified email alone cannot replace an existing sign-in method.

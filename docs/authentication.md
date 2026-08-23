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
3. The activation page displays the device and Ed25519 public-key fingerprint.
   Only an explicit same-origin POST carrying an authenticated, expiring
   confirmation bound to the authorization row, code, mode, and public-key
   digest may start Authorization Code flow with PKCE. A GET cannot approve or
   start identity authorization.
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

Copy `infra/identity/env.example` to an operator-owned mode-0600 environment
file, replace every invalid placeholder with a reviewed immutable
`repository@sha256` digest, validate it, and generate root-owned secrets:

```sh
sudo install -d -m 0700 /etc/blazn/identity/secrets
sudo ./infra/identity/validate-environment.sh /etc/blazn/identity/env
sudo ./infra/identity/generate-secrets.sh \
  /etc/blazn/identity/secrets admin@your-company.example
```

All identity mutation scripts reject `/`, non-canonical dot segments, paths
outside their fixed production/disposable prefixes, nested or overlapping
data/secrets/backup/receipt roots, and any symlink component before mutation.
The generator also rejects non-root execution, non-regular
or multiply-linked secrets, and owner/mode drift. Writes use owner-only
same-directory temporaries, atomic rename, and filesystem sync. The generated
ZITADEL master key is not rotatable in place. Back it up before
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
ZITADEL_REVIEWED_RELEASE=v4.17.1
ZITADEL_REVIEWED_ASSURANCE_POLICY_DIGEST=sha256:<review-receipt-digest>
ZITADEL_REVIEWED_ACR_VALUES=<exact-ZITADEL-ACR>
ZITADEL_REVIEWED_MFA_AMR_SETS=pwd+otp;pwd+webauthn
OIDC_COOKIE_KEY_FILE=/run/secrets/oidc_cookie_key
```

These values and secrets are intentionally absent from the base control-plane
Compose file. After the qualification gate passes, install the eight exact
nonsecret entries from `infra/milestone-2/env.identity.example` in the
root-owned `/etc/blazn/identity/control-api.env`. Persist these two entries in
the systemd-loaded `/etc/blazn/control-plane/control-plane.env`:

```text
BLAZN_IDENTITY_ENABLED=true
BLAZN_IDENTITY_ENV_FILE=/etc/blazn/identity/control-api.env
```

Reconcile the ownership receipt under the control-plane mutation lock, then
restart `blazn-control-plane.service`. The lifecycle scripts use a canonical
snapshot of the validated identity environment for build, start, supervision,
preflight, and stop; a systemd restart therefore preserves the overlay. The
enabled state, all reviewed nonsecret identity values, and both secret-file
digests are part of the live control-plane configuration digest.

Rollback is explicit: set `BLAZN_IDENTITY_ENABLED=false`, reconcile the receipt
under the same approval and mutation lock, then restart the unit. The disabled
digest and base Compose invocation are distinct from the enabled deployment.

## Registration, social identity, and MFA policy

Enable self-registration and verified email. Configure Google, GitHub, and
Apple as instance-level external identity providers, but do not enable automatic
linking by email. A matching verified email is not sufficient proof that a new
social identity owns an existing Blazn account.

Require MFA for Blazn. Prefer passkeys/WebAuthn, allow TOTP as the recovery-
compatible fallback, and require recovery setup before production access. SMS
is not a primary factor.

The callback intentionally fails closed unless the identity token has verified
email, an exact ACR from the reviewed ZITADEL release, and every method in one
reviewed multi-method AMR set. A generic `mfa`, `passwordless`, or other single
AMR value is insufficient. The release, assurance-policy digest, ACR, and AMR
set are recorded on the device authorization for audit.

## Backup, restore, and exact rollback

The identity backup includes a clean PostgreSQL dump, root-owned secrets and
master key, the private login-client PAT volume, immutable image environment,
and exact Compose/proxy/config definitions under one checksum manifest:

```sh
sudo ./infra/identity/backup.sh /etc/blazn/identity/env /srv/backups/blazn/identity/001
sudo ./infra/identity/restore.sh /srv/backups/blazn/identity/001 /etc/blazn/identity/env
```

Restore refuses checksum drift, a different image environment, or different
checked-out deployment definitions. Pre-restore database and secret trees are
moved aside rather than deleted. Before `down -v`, restore snapshots and
checksums the currently running login-client PAT volume. Any failed forward
restore invokes the independently tested PAT repair path; the same verified
repair command can roll backward to the pre-restore snapshot or forward to the
backup snapshot. The disposable qualifier verifies the restored master-key and
PAT-volume digests and retains a recoverable pre-restore tree until cleanup.

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

`infra/identity/test-disposable.sh` is the fail-closed dynamic gate. It requires
reviewed image digests, `/tmp`-scoped disposable roots, a real TLS issuer, the
control API overlay, and an executable browser/email/MFA driver that emits the
strict qualification receipt. It exercises Compose bootstrap/readiness, exact
OIDC discovery and PKCE, verified email, reviewed ACR plus multi-method MFA,
legacy login, explicit device confirmation, OIDC-aware health, backup/restore,
exact image rollback, master-key recovery, and PAT-volume recovery. Absence of
the reviewed images, mail delivery, provider/bootstrap configuration, or driver
is a hard blocker rather than a skipped green gate.
The browser driver must be a root-owned, single-link mode-0500/0700 file at the
fixed driver path and must match a separately reviewed SHA-256 digest. It emits
per-gate evidence digests and timestamps, not self-authored pass booleans. The
harness independently constructs the final receipt and binds the reviewed
issuer and operator environment digest. Each Compose service is recorded by
service name with its configured repository@manifest digest, exact before/after
container IDs, observed `Config.Image`, and image ID; rollback requires the
configured and image identities to match while the recreated container identity
changes. The backup utility has a separate configured repository@manifest and
observed image-ID binding, preventing a substituted restore helper. The receipt
also binds backup/database/PAT/master-key digests, driver digest, and gate
evidence. OIDC-aware health uses a bounded ten-second cache and
singleflight probe; discovery, JWKS, and token responses are streamed under a
hard one-megabyte cap so health traffic cannot amplify provider responses.
The disposable environment must therefore set
`ZITADEL_QUALIFICATION_DRIVER_SHA256` and a new receipt path under
`/var/lib/blazn/identity-qualification/`; neither the environment nor receipt
may be nested under any disposable data, secrets, backup, or recovery root.
Temporary databases, secrets, PAT volumes, and secret-bearing backup archives
are removed on every exit; only the validated redacted receipt is installed at
the new root-owned `BLAZN_IDENTITY_QUALIFICATION_RECEIPT` path.

ZITADEL is AGPL-3.0 licensed. Running the unmodified upstream images is isolated
from Blazn source, but any ZITADEL modifications must receive an explicit
license/compliance review and corresponding-source plan before deployment.

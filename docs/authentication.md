# Blazn authentication

Blazn signs users in with passwordless email codes. The hosted API
(`https://api.blazn.frontro.com`) runs with no external identity provider:
its health check reports `"identityProvider": "disabled"`. Accounts,
sessions, and device credentials live in the Blazn database.

## Request flow

1. `blazn auth login` creates a device-bound authorization in the Blazn API.
2. The CLI opens the `/activate` page and displays the same one-time code shown
   in the browser. The page shows the device and its Ed25519 public-key
   fingerprint.
3. The user enters an email address and chooses sign-up or sign-in. The API
   emails a six-digit code through Resend. Only an HMAC of the code is stored,
   bound to the authorization and address; it expires after 10 minutes and
   allows five attempts.
4. A correct code creates the account (sign-up) or confirms the existing one
   (sign-in) and approves exactly that pending device authorization.
5. The CLI proves possession of its Ed25519 private key and receives a
   Blazn-scoped, revocable device session.

Codes for the reserved qualification domain configured in
`EMAIL_CAPTURE_DOMAIN` (an RFC 2606/6761 `.invalid`, `.test`, or `.example`
domain) are delivered to a private capture inbox instead of Resend, so
automated qualification can sign in without a real mailbox.

The control API still contains the ZITADEL authorization-code integration
behind its `ZITADEL_*` settings, but no identity stack is deployed or
maintained in this repository.

## Headless macOS credential storage

Interactive macOS sessions use the login Keychain by default. A Mac used only
through SSH can have a login Keychain that exists but rejects non-interactive
access with `errSecInteractionNotAllowed`. Choose the built-in protected file
backend before the first login on such a host:

```sh
BLAZN_DARWIN_CREDENTIAL_BACKEND=protected-file blazn auth login --no-browser
```

The choice is written to an origin-namespaced, mode-0600 backend receipt under
the current user's mode-0700 Blazn credential directory. Later commands use
that receipt without the environment variable. The protected credential itself
is a direct, owner-only, no-symlink mode-0600 file with atomic replacement and
directory synchronization. A conflicting or unsupported backend override fails
closed. Select one backend before storing credentials; do not alternate between
Keychain and protected-file storage for the same API origin.

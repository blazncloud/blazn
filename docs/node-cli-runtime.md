# Node CLI/install/daemon runtime

The Node runtime generates one local Ed25519 identity and stores it in an
atomic, create-once `0600` file under a private `0700` directory. Enrollment
persists the authenticated plan-signing key tuple before token exchange, never
persists the enrollment token, and verifies the returned plan with the
generated strict contract implementation plus a local trusted-install profile.
The local profile binds the measured current executable, embedded component
digests, allowed origins and redirect suffixes, mutation roots, symlink checks,
and (when applicable) the exact Lima VM/worker material. It also pins one exact
`controlPlaneOrigin`: an HTTPS origin with no credentials, path, query, or
fragment. This is the TLS trust anchor used by the privileged adapter to replay
the enrollment exchange independently.

When installation is requested, the service hands the validated enrollment ID,
one-time token, exact exchange input, signing-key tuple, and expected response
directly to `Platform.AuthorizeBootstrap` in memory after exchange and before
any install operation. The token is excluded from JSON and redacted from Go
string formatting; it is never added to runtime state, the install WAL, a
receipt, CLI output, arguments, environment variables, or logs. A platform that
cannot authorize this bootstrap fails closed. Successful implementations write
only the token-free, digested `blazn.dev/node-root-install-authority/v1` record
to root-owned state. That authority includes the exact plan-signing public-key
tuple, Node public key, and the replay-authenticated nullable Kubernetes binding
so every later privileged operation can recompute both fingerprints, verify the
stored plan signature and current expiry against the root-owned profile, re-prove
the adopted Node UID/resourceVersion, and avoid the service-owned enrollment pin
entirely.

Privileged mutation is exposed through the narrow `node.Platform` interface.
Before each mutation, prior state and rollback material are durably written to
an exclusive install WAL. A `pending` record is conservatively treated as
possibly applied after a crash. Rollback is reverse-ordered and idempotent;
residue produces a signed `recovery_required` receipt. A verified install marks
the WAL complete before publishing a deterministic signed active receipt, so a
crash cannot turn a completed install into a rollback.

The daemon skeleton reloads the enrolled private identity for every heartbeat,
checks its fingerprint and expiry, binds host/worker capability to the verified
plan, computes the frozen capability digest, and signs canonical JSON with the
node-proof prefix. A new process uses a new boot ID and starts sequence zero.

The concrete isolated HTTP replay and root-owned authority persistence are
implemented by the privileged Linux/macOS adapter milestone. This contract
does not permit a platform to trust the service-owned enrollment pin/runtime
as root authority and never substitutes unprivileged or unmanaged mutations.
Linux privileged state is rooted at `/var/lib/blazn-node-root`; macOS privileged
state is rooted at `/Library/Application Support/BlaznNodeRoot`. Both are
root-owned mode `0700` and contain authority, install WAL/receipts, and rollback
backups. Daemon identity/runtime state remains in the separate service-owned
platform path.

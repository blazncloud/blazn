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
the adopted Node name and UID, and avoid the service-owned enrollment pin
entirely. Kubernetes resourceVersion is short-lived concurrency evidence: it is
checked for each cluster mutation and refreshed after that mutation, never used
as long-lived node identity.

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
`node serve` constructs this runtime solely from the finalized service-owned
identity, runtime record, and its persisted HTTPS control-plane origin; it does
not open a workspace session or load a user access token. Host totals come from
the host, while worker CPU, memory, and ephemeral storage come exclusively from
Kubernetes `status.allocatable` through the fixed no-input `node-root-observe`
helper. The service identity's receipt-bound sudo rule authorizes only that
subcommand and exposes no kubeconfig, Lima credential, or general root-helper
operation. Observation remains available after the short install-plan and
initial identity lifetimes only while the exact root authority, active signed
receipt, and live Kubernetes Node UID still agree.

`node repair` requires the still-current reviewed plan and preserves the
original uninstall prior-state evidence while journaling repair-time rollback
material separately. `node uninstall --yes` rolls back receipt-owned service,
eligibility, identity, and state mutations; package/image runtime is preserved
unless `--remove-managed-runtime` is explicitly supplied. Apart from the fixed
active-receipt observation above, an expired plan can authorize only rollback
backed by the exact root WAL or terminal receipt, not new apply, repair, join,
or capture work. Successful uninstall can therefore be
followed by a clean enrollment and reinstall.
Repair WALs embed the original signed active receipt. A failed or interrupted
repair restores its repair-time captures, republishes that original receipt,
and never reports the Node removed. Install recovery checkpoints persist the
non-secret broker issuance ID and reconcile issue, host join, root binding,
broker consumption, verification, and receipt publication. An exact joined UID
that cannot be removed remains bootstrap-tainted and is reported as a
`recovery_required` quarantine residue.
Residues are cumulative WAL evidence and survive every recovery retry. Before
reporting an uninstall or rolled-back install as removed, the runtime writes a
token-free service-state cleanup journal containing the verified plan and
pre-signed terminal receipt, removes the observation policy and private local
identity/runtime/pin state through idempotent checkpoints, publishes the root
receipt, and only then deletes the root WAL and local journal. A joined worker
that survives rollback is atomically re-tainted with the exact bootstrap
`NoSchedule` taint under UID/resourceVersion CAS before quarantine is reported.

The concrete isolated HTTP replay and root-owned authority persistence are
implemented by the privileged Linux/macOS adapter milestone. This contract
does not permit a platform to trust the service-owned enrollment pin/runtime
as root authority and never substitutes unprivileged or unmanaged mutations.
Linux privileged state is rooted at `/var/lib/blazn-node-root`; macOS privileged
state is rooted at `/Library/Application Support/BlaznNodeRoot`. Both are
root-owned mode `0700` and contain authority, install WAL/receipts, and rollback
backups. Daemon identity/runtime state remains in the separate service-owned
platform path. macOS trusted install profiles use the separate installer-owned
mode-`0700` `/Library/Application Support/BlaznNodeProfiles` root so an
unprivileged authenticated installer can read a reviewed profile without making
the privileged receipt root traversable. The privileged helper is the hidden `node-root-helper` subcommand
of the receipt-owned `/usr/local/bin/blazn` executable, so the supported path
does not depend on a second undistributed binary.

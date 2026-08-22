# Node CLI/install/daemon runtime

The Node runtime generates one local Ed25519 identity and stores it in an
atomic, create-once `0600` file under a private `0700` directory. Enrollment
persists the authenticated plan-signing key tuple before token exchange, never
persists the enrollment token, and verifies the returned plan with the
generated strict contract implementation plus a local trusted-install profile.
The local profile binds the measured current executable, embedded component
digests, allowed origins and redirect suffixes, mutation roots, symlink checks,
and (when applicable) the exact Lima VM/worker material.

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

This milestone intentionally provides mock platform/capability adapters only.
The real Linux and macOS privileged adapters, their OS-specific local trusted
profile packaging, and live host installation are separate acceptance work.
The production CLI factory therefore fails closed until those adapters are
injected; it never substitutes unprivileged or unmanaged mutations.

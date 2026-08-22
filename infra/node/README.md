# Node broker infrastructure prerequisites

This directory provisions the prerequisite identity and keys required before
`004_nodes.sql` may run. It does not implement the Node API, CLI, installer, or
daemon.

The fresh-host path is integrated into Milestone 2 host preparation. It creates
the authenticated least-privilege `blazn_node_broker` login, a database URL,
the raw 32-byte enrollment HMAC key, and the raw 32-byte AES-256-GCM join-key at
the paths frozen by `docs/node-contract.md`. The enclosing directory and both
cryptographic keys are root-only. Only the database URL is mounted into the
pre- and post-migration verification jobs. The control API, bootstrap job, and
migrator never receive either cryptographic key or the broker credential.

`verify-database.sh pre-migration` authenticates as the broker and rejects a
missing role, unsafe role attributes or membership, missing CONNECT/USAGE, or
schema CREATE. After migration 004, `post-migration` additionally proves the
exact positive and negative table privilege matrix. The API bootstrap depends
on that post-migration gate.

The control-plane ownership receipt stores only SHA-256 digests and key IDs.
Backups bind their metadata to the canonical digest of that receipt section;
no Node broker secret is copied into backup evidence. A restore qualification
must be supplied the matching ownership receipt and a separately protected key
inventory containing the database URL, enrollment HMAC key, and join-credential
key. Restore fails before creating a database when metadata, receipt, or key
digests differ, because encrypted join issuances are not recoverable without the
exact AES key generation.

Fresh and upgrade secret creation share a journaled state machine. Every file is
written to a unique temporary tree, fsynced, validated for length/ownership/mode,
and atomically published. The live upgrade also backs up the exact environment,
build receipt, source/config bindings, and ownership receipt before modifying
them. It writes the Compose secrets-root setting and reconciles the build and
config receipt itself, so systemd does not depend on a manual environment edit.
Rollback is a crash-resumable journal: database grants and owned dependencies
are cleared transactionally before the role is dropped, secrets are moved to a
recoverable receipt-bound location, and the prior environment/build/ownership
inputs are atomically restored.
The final rollback phase is deliberately `source-restore-required`: the
operator must restore the release tree whose source and configuration digests
are recorded in the upgrade receipt, then rerun the rollback command with that
restored `infra/milestone-2` path. The journal refuses `rolled-back` until both
digests match, ensuring the restored build and ownership receipts can actually
pass startup preflight rather than pointing at the newer source tree.

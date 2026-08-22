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
no Node broker secret is copied into backup evidence.

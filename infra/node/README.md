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

## Install-plan signing material

The same prerequisite step atomically creates `/etc/blazn/node-plan`. Its
private signing key is a raw 32-byte Ed25519 seed encoded as one 43-character,
unpadded base64url line. The stable key ID is
`control-plane-node-plan/v1`; `signing-public-v1.json` records the matching raw
public key and its lowercase SHA-256 fingerprint. Ownership, upgrade, backup,
restore, and rollback evidence binds the public fingerprint and template digest,
never a digest or copy of the private seed.

`node-install-plan-template-v1.json` is a closed bundle with exactly the fresh
Ubuntu 26.04 AMD64, existing Linux adoption, and macOS/Lima adoption profiles.
It freezes the current Frontro MicroK8s identity and CA, worker-only boundary,
release artifacts and checksums, registry trust, platform service definitions,
resource bounds, ordered mutations, validation gates, and rollback roots.
The Linux profiles use architecture-specific official Snapcraft API material:
revision 9072 for AMD64 and revision 9075 for ARM64. The local
installer profile pins `.cdn.snapcraftcontent.com` as a redirect-only suffix;
the initial API origin remains exact, and every hop still undergoes HTTPS,
userinfo, DNS/IP-policy, size, and digest validation.

Only the long-running API and one-shot `node-plan-verify` gate receive the
private seed. The migrator, bootstrap, broker checks, object services, and
identity tools do not. Before API startup, the gate reconstructs the Ed25519
key, proves the public fingerprint, validates all three profiles, and checks the
checked-in systemd and launchd definition digests.

The raw seed is recovered separately from ordinary database/object evidence.
The root-only recovery inventory supplied to restore qualification contains
`signing-private-v1.b64url`, `signing-public-v1.json`, and the exact template in
addition to the broker inventory. Restore derives the fingerprint from the seed
and rejects fingerprint or template drift. Upgrade rollback retains the raw
seed beneath its root-only, receipt-named recovery directory instead of deleting
it.

To advance the reviewed executable pinned by an existing plan, stop the control
plane outside its lock, then run `rotate-plan-materials.sh` through
`with-control-plane-env.sh` and `with-control-plane-lock.sh`. Set
`BLAZN_CORRELATION_ID` and `BLAZN_NODE_PLAN_TEMPLATE_SOURCE` to the template in
the immutable promoted release. The crash-resumable rotation retains exact
before/after images under `/var/lib/blazn/ownership/node-plan-material-rotations`,
atomically replaces the template, creation journal, upgrade receipt, and main
receipt in that order, and never replaces the signing key. On failure, rerun the
same command and correlation ID, or pass `--rollback` before restoring the old
release. Start systemd only after rotation, release promotion, and deploy
preflight finish; do not stop or start systemd while holding the control-plane
lock.

## MicroK8s worker issuer boundary

`install-worker-issuer.sh` provisions the root helper, distinct HMAC
generation, closed config, broker socket group, systemd/tmpfiles policy,
recovery inventory, and crash-resumable receipt. The broker Compose profile
receives only its database URL, AES join key, and fixed Unix socket—never the
issuer HMAC key, Docker socket, kubeconfig, or MicroK8s directory.
`upgrade-worker-issuer-observation.sh` transactionally moves an existing
blocked receipt to the observation-enforced binary while preserving recovery
material. Live use still requires the disposable-node qualification in
`docs/microk8s-worker-issuer-infra-runbook.md`.

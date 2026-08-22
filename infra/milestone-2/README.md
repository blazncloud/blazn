# Milestone 2 control-plane infrastructure

This directory defines the ben1-local PostgreSQL, S3-compatible object store,
control API build hook, and ngrok public-origin boundary for the POC. It is
deliberately not a production HA or disaster-recovery design.

No script in this directory is run by repository checkout or CLI installation.
Host mutation requires root, the shared fenced control-plane lock, an explicit
environment, and a successful preflight. `preflight.sh --plan` is read-only.

## Network and service boundary

- PostgreSQL binds only to `127.0.0.1:55432` on ben1.
- The S3 API and administration console bind only to `127.0.0.1:59000` and
  `127.0.0.1:59001`.
- The API binds only to `127.0.0.1:58080` and is the sole ngrok target.
- The Compose bridge is pinned to the existing `172.18.0.0/16` topology. The
  API accepts forwarded identity from gateway `172.18.0.1/32` only when exactly
  one proxy hop and the dedicated ngrok authentication header both validate.
  Missing, duplicate, or incorrect authentication is rejected before rate
  accounting rather than sharing the bridge-gateway identity.
- Containers share a private Compose bridge. Secret values are named files
  inside a root-only directory and only those files are mounted into their
  declared containers through Compose secrets; they are not ordinary resource
  metadata.
- PostgreSQL and MinIO images are versioned and pinned to reviewed manifest-list
  digests. The API build is sourced from `services/control-api` and uses that
  service's own Node 22 Dockerfile and `CMD`.

The long-running TypeScript API contract is `GET /healthz` plus `PORT`,
`BIND_ADDRESS`, `PUBLIC_URL`, `DATABASE_URL_FILE`, `S3_ENDPOINT`, `S3_REGION`,
`S3_BUCKET`, `S3_ACCESS_KEY_FILE`, and `S3_SECRET_KEY_FILE`. It receives only
the DML-only runtime database URL. A one-shot migration service receives the
non-superuser schema-owner URL, then a one-shot bootstrap service receives the
dedicated bootstrap URL and initial-login password. Neither elevated secret reaches the
runtime API.

Fresh PostgreSQL initialization creates four distinct identities: the
container-only administrative user, `blazn_migration` as the non-superuser
database/schema owner, `blazn_bootstrap` for the initial identity only, and
`blazn_runtime` for request handling, plus `blazn_node_broker` for the narrow
Node join-issuance boundary. The broker authenticates through its own root-owned
database URL. A pre-migration gate proves its restricted role attributes,
CONNECT, and schema USAGE; a post-migration gate proves the exact positive and
negative grants from `004_nodes.sql` before API bootstrap. The Node enrollment
HMAC and join-credential AES-256-GCM keys remain root-owned and are not mounted
into the current API, bootstrap, or migration services. Migration SQL grants table-specific
operations to bootstrap and runtime; initialization does not grant blanket
future-table DML.

The object service has its own restart-idempotent one-shot initializer. It uses
the pinned `mc` image and root credential to create the required bucket and a
bucket-scoped runtime identity. The API and backup tooling receive only that
runtime identity. The API performs an authenticated signed `HeadBucket`, so a
healthy process also proves its bucket access.

## Required decisions before a ben1 deployment

1. Approve the exact `/srv/frontro/blazn-poc/control-plane` mount and confirm it
   is not the root filesystem.
2. Select `BLAZN_BACKUP_ROOT` on a genuinely separate failure domain. The
   preflight rejects a destination on the same filesystem; the POC must not call
   a same-host-only copy disaster recovery.
3. Confirm the reserved loopback ports, resource limits, storage thresholds,
   database name, bucket name, system user/group plan, and ngrok hostname have no
   collisions.
4. Provision the ngrok credential and DNS/domain mapping without exposing any
   database or object-store endpoint.
   The live environment must use `PUBLIC_URL=https://blazn.benpelo.com`; local
   tests may retain the loopback default.
5. Review the image digests and MinIO's AGPLv3 obligations before deployment.

The exact pinned MinIO image was inspected for Linux AMD64 and contains
`/usr/bin/curl`; its health check uses the unauthenticated local
`/minio/health/live` endpoint. Image qualification must repeat for ARM64 before
that architecture is used for this service.

## Controlled lifecycle

For the current POC fleet, `backup-nfs/ben4.exports` and the matching mount
unit provide the reviewed separate backup failure domain: ben4 exports only the
dedicated backup directory to ben1's private-LAN address, and ben1 mounts it with
`nosuid,nodev,noexec`. These files are host-specific evidence, not portable
defaults; a production deployment must provision independent durable storage.
Preflight and every backup verify the exact active NFS mountpoint, source, and
filesystem type so an unmounted `/mnt` directory can never receive a backup.

The intended installer-owned sequence is:

```text
preflight.sh --plan
with-control-plane-lock.sh dependency <correlation-id> auto install-compose-plugin.sh
with-control-plane-lock.sh ngrok-user-install <correlation-id> auto install-ngrok-user.sh
with-control-plane-lock.sh prepare <correlation-id> auto prepare-host.sh
install dedicated `/etc/blazn/ngrok` as root:blazn-ngrok mode 0750 and
`ngrok.yml` as root:blazn-ngrok mode 0640
install reviewed files and systemd unit
systemctl enable --now blazn-control-plane.service
health, migration, restart-idempotent bootstrap and object initialization, S3
checksum, restart, auth, and revocation tests
with-control-plane-lock.sh backup <correlation-id> auto backup.sh <correlation-id>
restore-test.sh <backup> /var/tmp/blazn-restore/<unique-id>  # isolated host only
```

For an existing installation or reviewed source update, run
`build-control-api.sh` under the control-plane lock, review its source/image
receipt, then run `update-receipt-config.sh` in a separate lock operation before
starting the service. Startup rebuilds and verifies but never silently
reconciles a changed image into the main receipt.

The dependency installer places the pinned Compose plugin under the dedicated
`/etc/blazn/docker-cli` configuration root. The systemd unit sets that exact
`DOCKER_CONFIG`, and backup/verification scripts set it internally, avoiding a
user's Docker CLI configuration and plugins.

Systemd is the sole restart owner. Compose containers never restart themselves;
the unit acquires the authoritative control-plane lock while preflight,
migration, bootstrap, bucket initialization, and health complete. Its
foreground process is monitor-only; systemd reacquires the same lock before
stopping the exact project and restarting it after a failure.

The API image is built under that same startup lock with the reviewed Compose
build. Its source digest covers the Dockerfile, package manifests, TypeScript
source, migrations, and public contracts. A root-owned build receipt binds that
digest to a `blazn-control-api:source-<digest>` tag, resulting Docker image ID,
and the main ownership receipt. Source digests must match before and after the
candidate build; a race or conflicting immutable tag preserves the prior tag
and receipt. Startup and the continuous supervisor verify all three API service
containers use the receipt-bound image ID.

Every API deploy/restart, schema migration, PostgreSQL/object-store restart,
backup promotion, and production-like restore must use the same
`ben1-control-plane-mutation` lock. Ngrok activation additionally requires the
separate `public-origin/blazn.benpelo.com` owner. The monotonically increasing
fencing token is passed to the foreground operation.

Both ngrok units execute the public-origin wrapper as their foreground process,
so the host-wide lock is held for the full tunnel lifetime rather than only for
the `systemctl start` request.

Blazn does not modify or reuse the existing HomeAI user tunnel. Its system units
use a dedicated non-login `blazn-ngrok` account and a separate root-controlled
ngrok configuration. Before dropping privileges, a root-only helper removes any
client-supplied `x-blazn-proxy-authorization` header and writes a Traffic Policy
that injects the 64-hex workspace secret. Only the policy-file path appears in
ngrok argv; the generated file and directory are inaccessible to ordinary ben1
users. This follows ngrok's documented `--traffic-policy-file` and ordered
remove/add header actions:
<https://ngrok.com/docs/agent/cli> and
<https://ngrok.com/docs/traffic-policy/actions/add-headers>.

`systemd/blazn-ngrok-qualification.service` is a temporary fallback for the
POC test matrix when the requested custom hostname has not yet been reserved.
It exposes the same API-only loopback target at an ngrok-assigned TLS URL, is
never enabled at boot, and does not close the named-hostname acceptance gate.

`prepare-host.sh` refuses pre-existing unowned data or secret paths and writes
the ownership receipt last. Cleanup is intentionally not automated here: a
reviewed cleanup must enumerate the exact receipt paths, reject links/mount
surprises, decide retain/export/delete for each data class, and obtain separate
confirmation before deletion.

Existing v1 ben1 installations use `upgrade-live-v1-to-v2.sh` once under the
control-plane lock before starting the hardened Compose project. It validates
the old receipt and secrets, preserves the current MinIO root credential under
the new names, stages and atomically links newly generated bootstrap/runtime
credentials, and reconciles the restricted `blazn_bootstrap` role through the
exact running PostgreSQL service. Its separate digest-only upgrade receipt makes
power-loss retries deterministic and never replaces the main ownership receipt.
Migration `002` must revoke v1's persisted default table/sequence privileges,
revoke existing broad table grants, and then apply explicit table grants. The
operator reconciles the main receipt in a separate reviewed lock operation.

## Evidence and tests

- `tests/test-preflight.sh` proves fail-closed behavior for same-filesystem
  backups, occupied ports, public binds, mutable images, and nested paths.
- `tests/test-contract.sh` checks loopback-only ports, the API environment/build
  contract, the single ngrok target, and shell syntax.
- `verify-object-store.sh` uses a run-unique prefix, compares the uploaded and
  downloaded SHA-256 digest, deletes exactly that prefix, and proves no residue.
- `backup.sh` creates a PostgreSQL custom-format dump, mirrors the object bucket,
  compares authenticated bucket listings before and after the database/object export,
  writes a checksum manifest, and atomically promotes the staging directory.
  Milestone 2 has no database object-metadata table, so this proves bucket
  stability rather than cross-store referential consistency. The first such
  schema must introduce an application-level snapshot barrier.
- `restore-test.sh` refuses ben1 and any non-disposable target path, verifies all
  backup checksums, requires a canonical direct child of the restore directory,
  restores to an isolated database, and retains evidence.

Run the non-mutating test suite on Linux:

```sh
infra/milestone-2/tests/test-preflight.sh
infra/milestone-2/tests/test-contract.sh
shellcheck infra/milestone-2/scripts/*.sh infra/milestone-2/tests/*.sh
```

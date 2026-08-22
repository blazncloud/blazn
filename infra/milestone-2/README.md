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
runtime URL and initial-login password. Neither elevated secret reaches the
runtime API.

Fresh PostgreSQL initialization creates three distinct identities: the
container-only administrative user, `blazn_migration` as the non-superuser
database/schema owner, and `blazn_runtime` with connect, schema usage, table
DML, and sequence-use privileges only. Default privileges make future objects
created by migrations available to runtime without granting runtime DDL.

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

The intended installer-owned sequence is:

```text
preflight.sh --plan
with-control-plane-lock.sh dependency <correlation-id> auto install-compose-plugin.sh
with-control-plane-lock.sh prepare <correlation-id> auto prepare-host.sh
install reviewed files and systemd unit
systemctl enable --now blazn-control-plane.service
health, migration, S3 checksum, restart, auth, and revocation tests
with-control-plane-lock.sh backup <correlation-id> auto backup.sh <correlation-id>
restore-test.sh <backup> /var/tmp/blazn-restore/<unique-id>  # isolated host only
```

The dependency installer places the pinned Compose plugin under the dedicated
`/etc/blazn/docker-cli` configuration root. The systemd unit sets that exact
`DOCKER_CONFIG`, avoiding a user's Docker CLI configuration and plugins.

Every API deploy/restart, schema migration, PostgreSQL/object-store restart,
backup promotion, and production-like restore must use the same
`ben1-control-plane-mutation` lock. Ngrok activation additionally requires the
separate `public-origin/blazn.benpelo.com` owner. The monotonically increasing
fencing token is passed to the foreground operation.

`prepare-host.sh` refuses pre-existing unowned data or secret paths and writes
the ownership receipt last. Cleanup is intentionally not automated here: a
reviewed cleanup must enumerate the exact receipt paths, reject links/mount
surprises, decide retain/export/delete for each data class, and obtain separate
confirmation before deletion.

## Evidence and tests

- `tests/test-preflight.sh` proves fail-closed behavior for same-filesystem
  backups, occupied ports, public binds, mutable images, and nested paths.
- `tests/test-contract.sh` checks loopback-only ports, the API environment/build
  contract, the single ngrok target, and shell syntax.
- `verify-object-store.sh` uses a run-unique prefix, compares the uploaded and
  downloaded SHA-256 digest, deletes exactly that prefix, and proves no residue.
- `backup.sh` creates a PostgreSQL custom-format dump, mirrors the object bucket,
  writes a checksum manifest, and atomically promotes the staging directory.
- `restore-test.sh` refuses ben1 and any non-disposable target path, verifies all
  backup checksums, restores to an isolated database, and retains evidence.

Run the non-mutating test suite on Linux:

```sh
infra/milestone-2/tests/test-preflight.sh
infra/milestone-2/tests/test-contract.sh
shellcheck infra/milestone-2/scripts/*.sh infra/milestone-2/tests/*.sh
```

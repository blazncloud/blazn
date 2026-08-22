# Milestone 2 — Control Plane and Authentication

## Outcome

Milestone 2 delivers the first Blazn management control plane and device-bound
CLI authentication flow. The service is running on ben1 with PostgreSQL and
S3-compatible object storage, and its backup is stored on a separately mounted
ben4 failure domain.

The implementation and temporary public-TLS qualification are complete. The
requested permanent origin remains an external activation gate:
`blazn.benpelo.com` must be reserved in the existing ngrok account before the
reviewed `blazn-ngrok.service` can start. Ngrok currently returns
`ERR_NGROK_319`, and DNS has no record for the hostname. The temporary
qualification service does not close that named-origin gate.

## Delivered

- Contract-first device authorization, current-user, device, refresh, logout,
  and revocation API resources.
- Deterministic OpenAPI-to-Go client generation with a CI drift check.
- Ed25519 proof of device possession for authorization exchange and refresh.
- Short-lived access credentials, rotating refresh credentials, hashed server
  storage, and terminal revocation handling.
- CLI `auth login`, `status`, `logout`, `devices`, and `revoke-device` commands.
- Native macOS Keychain, Linux Secret Service, and protected standalone Linux
  credential storage without writing tokens to Blazn configuration.
- PostgreSQL 17.6 with separate migration-owner and DML-only runtime roles.
- MinIO object storage with an idempotent bucket initializer and authenticated
  readiness checks.
- Pinned Node 22 control API image and pinned, receipt-owned Docker Compose.
- Loopback-only host bindings; ngrok exposes only the API listener.
- Fenced control-plane and public-origin mutation locks.
- Checksummed logical backup, object export, and isolated restore tooling.

## Live placement

- ben1 source: `/opt/blazn`
- ben1 data: `/srv/frontro/blazn-poc/control-plane`
- ben1 secrets: `/etc/blazn/control-plane/secrets`
- ben1 ownership receipt: `/var/lib/blazn/ownership/control-plane.json`
- ben1 API: `127.0.0.1:58080`
- ben1 PostgreSQL: `127.0.0.1:55432`
- ben1 S3 API/console: `127.0.0.1:59000` and `127.0.0.1:59001`
- ben4 backup export: `/srv/blazn-poc-backup`
- ben1 mounted backup: `/mnt/blazn-poc-backup`
- retained successful restore evidence:
  `/var/tmp/blazn-restore/m2-live-002` on ben4

The NFS export is restricted to ben1's primary private-LAN address and is
mounted `nosuid,nodev,noexec`. Its filesystem identity differs from the live
`/srv/frontro` data filesystem.

## Verification evidence

The combined committed source passed:

- Go race tests, vet, formatting, generated-client drift, Linux AMD64, Linux
  ARM64, and Darwin ARM64 builds on the remote build lanes.
- Pinned Node 22 TypeScript type-check, build, and unit tests.
- Infrastructure preflight, contract, ShellCheck, Compose semantic, PostgreSQL
  role-isolation, MinIO initialization, and object lifecycle tests.
- Live ben1 health with authenticated database and S3 readiness.
- Live Linux CLI device login, protected credential storage, current-user and
  device lookup, REST revocation, and immediate SSE revocation over an
  ngrok-assigned TLS endpoint.
- Live macOS ARM64 login, status, remote logout, and deletion through native
  Keychain APIs in an isolated unlocked test Keychain. The test Keychain was
  removed after qualification; the operator's login Keychain was not unlocked
  or modified.
- Fenced full service restart with checksum-verified migration reuse,
  restart-idempotent identity bootstrap, bucket initialization, and health.
- S3 upload/download SHA-256 equality and exact prefix residue absence.
- PostgreSQL logical backup on the ben4-backed mount and checksummed restore
  into an isolated ben4 container. The live database was never a restore
  target.

No credential value is included in repository files, command output, logs, or
this evidence. The initial local account password remains only in the
root-owned ben1 secret file.

## Permanent-origin activation

To close the remaining Gate 2 item:

1. Reserve `blazn.benpelo.com` in the existing ngrok account.
2. Add the exact DNS validation/routing record returned by ngrok.
3. Start `blazn-ngrok.service` under `with-public-origin-lock.sh`.
4. Verify public TLS health, browser activation, login, restart, and revocation
   through `https://blazn.benpelo.com`.
5. Stop the temporary qualification tunnel.

The existing `homeai-ngrok.service` remains active and unchanged throughout.

## Boundary

Workspace resources, memberships, invitations, roles, and cross-workspace
authorization are Milestone 2A. They are intentionally not hidden inside this
milestone's auth schema.

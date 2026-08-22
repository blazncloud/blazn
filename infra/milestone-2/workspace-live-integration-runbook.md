# Workspace live-integration qualification

**Target:** ben1 control plane at `https://blazn.benpelo.com`  
**Scope:** migration `003_workspaces.sql`, Workspace API/CLI, and two-user acceptance  
**Change model:** one operator, one serialized mutation at a time, controlled temporary identity

This is a reviewed operator runbook, not an automatic deployment script. Stop at
every hold point and retain command output as change evidence. Never modify,
stop, or reuse the existing HomeAI user tunnel. Blazn continues to use only its
dedicated system tunnel and public-origin lock.

## Preconditions and named evidence

- Record the exact release commit, API source digest, image ID, main ownership
  receipt digest, installed systemd unit digest, migration list, and the status
  of all Compose services.
- Confirm ben1 has sufficient capacity and that the approved ben4 NFS mount has
  the exact target, source, and `nfs4` filesystem recorded in the environment.
- Confirm the live API and Blazn tunnel are healthy and the HomeAI tunnel is
  unchanged before and after every service operation.
- The initial owner remains user A. The receipted `manage-poc-identity.sh`
  lifecycle creates user B with a scrypt record compatible with the API, keeps
  its password and profile in root-only named files, and uses a separate CLI
  home. Never improvise a SQL user or share user A's session.
- Use unique 8-to-128-character request IDs and a single correlation ID for the
  change. Never place an invitation token, password, session, or HMAC key in an
  argument, log, shell trace, clipboard transcript, or evidence bundle.
- The HMAC key is deliberately absent from database/object backups. Before the
  change, confirm the root-controlled configuration recovery process can retain
  `/etc/blazn/control-plane/secrets/workspace-invitation-hmac-v1` without
  exposing it. Backup metadata records only its SHA-256 digest.

## 1. Read-only qualification and pre-change backup

On ben1, capture read-only state without using `--plan` against occupied live
ports. Verify the exact NFS mount with `findmnt`, loopback listeners,
service state, current tunnel ownership, and free bytes/inodes. Do not infer
these facts from an earlier run.

Create a pre-change backup using the **currently installed** backup script. Load
and validate `/etc/blazn/control-plane/control-plane.env`, then acquire the exact
control-plane lock around the backup command. This matters because the incoming backup script
requires the new key and reconciled receipt. Promote no change until the backup
checksum passes and `restore-test.sh` has restored that backup on ben4 under a
unique direct child of `/var/tmp/blazn-restore`.

**Hold point A:** pre-change backup and isolated restore evidence are complete;
ben1 health and both tunnel ownership records are unchanged.

## 2. Stage the exact release

Do not copy a checkout over `/opt/blazn`. From a separate reviewed checkout,
invoke `stage-release.sh CHECKOUT FULL_COMMIT` through the validated environment
and exact lock. It uses `git archive`, writes a complete checksum manifest,
binds the commit/tree/API+migration/config/systemd digests in a release receipt,
and promotes only to an immutable direct child of `/opt/blazn-releases`.
Run the repository's infrastructure, API, Go, generated-client, and isolated
PostgreSQL checks on a non-live Linux host first.

Do not run Compose manually. Keep systemd as the sole restart owner. All ben1
release stage/promotion, build, secret, receipt, backup, stop, and start mutations use the same
`ben1-control-plane-mutation` wrapper and a recorded correlation ID.

Stop `blazn-control-plane.service` and confirm it is fully inactive. Then invoke
the staged candidate's `promote-release.sh FULL_COMMIT` under the exact lock.
Promotion accepts only the exact `inactive` unit state and independently rejects
any still-running container labelled as the receipt-owned `blazn-m2` Compose
project; `failed` is not treated as stopped.
The promotion first adopts a legacy `/opt/blazn` directory into a checksummed,
read-only prior release, writes a retryable promotion intent, atomically swaps
the `/opt/blazn` symlink, installs and byte-compares the candidate systemd unit,
and preserves the prior release for `rollback-release.sh`. Do not start the
service until the next two sections complete.

## 3. Install and bind the invitation key

With the service stopped and the candidate active, load the root-owned
environment and invoke the upgrade through the mutation lock:

```sh
sudo /opt/blazn/infra/milestone-2/scripts/with-control-plane-env.sh \
  /opt/blazn/infra/milestone-2/scripts/with-control-plane-lock.sh \
  workspace-secret <correlation-id> auto \
  /opt/blazn/infra/milestone-2/scripts/upgrade-live-v2-to-workspace.sh
```

The upgrade is retryable. It generates 32 random bytes as 64 lowercase hex
characters, installs a root-owned mode-`0444` named file inside a mode-`0700`
root directory, emits no key material, and records only its digest in the
separate upgrade receipt. That receipt also binds the correlation ID, fencing
token, and active immutable release digest. A pre-existing unreceipted key,
release change, or mismatched partial state fails closed.

Next, using `with-control-plane-env.sh` outside
`with-control-plane-lock.sh` for every command, run `build-control-api.sh`,
review its source/image receipt, and run `update-receipt-config.sh`. The main receipt
must now bind:

- the API source digest, including all migration files;
- the immutable API image and image ID;
- the full control-plane configuration digest; and
- the digest of `workspace-invitation-hmac-v1`.

Run `preflight.sh --deploy` through the same wrappers while the service remains
stopped. It must reject an absent,
mis-owned, malformed, rotated, or receipt-mismatched key.

**Hold point B:** both receipts validate, no secret was printed, and Compose
configuration shows the named key mounted only into the long-running `api`
service—not PostgreSQL, MinIO, migration, bootstrap, or tool containers.

## 4. Serialized migration and restart

Use `systemctl start blazn-control-plane.service`. The unit synchronously
acquires the same mutation lock, rebuilds/verifies the receipt-bound image,
runs `api-migrate`, runs the restart-idempotent bootstrap and object initializer,
waits for API health, and verifies every API container image ID. Do not issue a
second restart or Compose command while this operation is active.

Verify migration `003` is recorded exactly once. Run
`preflight.sh --existing-deploy` through the validated environment and exact
lock; unlike planning mode, it expects occupied receipt-bound loopback listeners
and verifies service labels, health, image IDs, published bindings, key receipt,
and installed systemd unit. Direct runtime-role SQL is used only to prove role
privilege boundaries. Tenant isolation is proved through authenticated API calls
in the next section.

**Hold point C:** migration, least-privilege, image, health, ngrok, and HomeAI
tunnel evidence all pass. Otherwise enter rollback and do not create workspace
data.

## 5. Two-user Workspace acceptance

Provision the controlled user B through the validated environment and exact
lock with `manage-poc-identity.sh provision`. It is retryable after interruption,
stores no plaintext credential in argv, logs, or receipts, and creates two
receipt-owned, non-login OS accounts with distinct passwd homes, UIDs, primary
groups, and no supplementary groups. Qualification invokes each CLI with
`setpriv --clear-groups --reset-env`, so Go's `os/user.Current()` resolves a
different real home and credential store for user A and user B. Environment
overrides are not treated as a credential boundary; inherited DBus and XDG
session variables are removed so both accounts use their own protected store.

Run `verify-live-workspace.sh /path/to/blazn https://blazn.benpelo.com` through
the same environment and lock. It authenticates user A and user B into separate
credential homes, creates two uniquely named `poc-company-*` workspaces, streams
the one-time token directly between CLI processes, records the exact workspace
IDs for cleanup, and makes all isolation decisions through the public API.
Before creating work, it proves user B's authenticated API user ID equals the
receipted POC identity and differs from user A; retained output contains only
redacted success evidence.

Then verify:

1. User B can list/get the joined workspace and cannot edit it, invite users,
   or manage membership while role `member` is active.
2. A second workspace owned by user A is indistinguishable from not found to
   user B across get, members, invitations, cursors, and event streams.
3. Start `blazn workspace watch` as user B. User A removes user B
   with the current membership version. The stream emits revocation/ends, a
   reconnect is denied, and all later workspace operations are denied.
4. A fresh invitation can re-add user B. Reusing a consumed, revoked, expired,
   or altered token fails; tokens never appear in invitation lists, audit rows,
   server logs, ngrok logs, or retained shell history.
5. Restart the service and repeat list/get/watch to prove persistence and
   restart-safe migration/bootstrap behavior.

Do not remove or demote the immutable initial owner during qualification.

## 6. Post-change backup and isolated restore

Create a new backup by invoking `backup.sh` through
`with-control-plane-env.sh` and `with-control-plane-lock.sh`. Its
`control-plane-backup/v2` metadata binds the config digest, API source/migration
digest, immutable image ID, and invitation-key digest without copying the key.
Verify checksums and run `restore-test.sh` on ben4. Compare the restored
workspace, memberships, consumed invitation state, idempotency receipts, and
redacted audit/event records with the live acceptance evidence.

Run `verify-rollback-inventory.sh <backup-directory>` as root against the exact
staged release before calling that backup rollback-compatible. It fails when
the release, migration source, image, receipt, config, or installed HMAC key
differs.

**Hold point D:** post-change backup, rollback inventory, isolated restore, and
all two-user checks pass. Only then mark live qualification complete.

After retaining the qualification backup, invoke `manage-poc-identity.sh
cleanup` through the environment and exact lock. It refuses any workspace
reference outside the receipted IDs, refuses a user-B-owned workspace, deletes
the exact `poc-company-*` resources, authorizations, devices, sessions, and user
in one database transaction, removes the isolated credential home, and retains
a redacted cleaned receipt.
Hashed authentication rate-limit rows are intentionally retained for the
server's bounded expiry cleanup because IP-scoped rows may be shared and the
account key includes a non-exported authorization ID; guessing or deleting by
plaintext login would not be exact cleanup.

## 7. Rollback boundary

Stop on any receipt, key, migration, role, API, tunnel, backup, or cross-workspace
failure. Preserve logs and incomplete staging evidence without secret values.

- Do not rotate or delete `workspace-invitation-hmac-v1`; doing so invalidates
  outstanding deterministic invitations. Preserve it across application
  rollback and restore.
- Prefer application rollback with the service stopped and
  `rollback-release.sh` under the validated environment and exact lock. It
  verifies the preserved release manifest, atomically swaps the active link,
  restores and compares that release's systemd unit, and records the reverse
  promotion. Keep migration `003` because it is additive; never reconstruct a
  prior release from a mutable checkout.
- Never invent a down migration. Database restore is a separate, destructive
  recovery decision because it discards post-backup changes. First repeat the
  restore on ben4, enumerate the exact ben1 target and backup, obtain explicit
  approval, stop the public service, and retain the failed live data for
  forensic recovery.
- A pre-change legacy backup does not contain the v2 rollback inventory. Its
  checksum and isolated-restore evidence remain required, and the same external
  root-controlled invitation key must still be retained if invitations were
  issued after the change.
- After rollback, repeat public TLS health/auth/revocation, PostgreSQL role,
  object-store, restart, backup, and HomeAI tunnel non-interference checks.

Record the final systemd state, container/image IDs, migration state, receipt
digests, backup path, isolated restore evidence path, and redacted two-user gate
results. Never retain raw credentials or invitation tokens in the evidence.

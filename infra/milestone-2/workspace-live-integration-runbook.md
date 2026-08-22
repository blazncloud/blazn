# Workspace live-integration qualification

**Target:** ben1 control plane at `https://blazn.benpelo.com`  
**Scope:** migration `003_workspaces.sql`, Workspace API/CLI, and two-user acceptance  
**Change model:** one operator, one serialized mutation at a time, no identity provisioning

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
- Identify two already-provisioned, distinct test users and two separate CLI
  credential stores. This runbook does not create users or share one user's
  device session with another user. Stop if the second identity does not exist.
- Use unique 8-to-128-character request IDs and a single correlation ID for the
  change. Never place an invitation token, password, session, or HMAC key in an
  argument, log, shell trace, clipboard transcript, or evidence bundle.
- The HMAC key is deliberately absent from database/object backups. Before the
  change, confirm the root-controlled configuration recovery process can retain
  `/etc/blazn/control-plane/secrets/workspace-invitation-hmac-v1` without
  exposing it. Backup metadata records only its SHA-256 digest.

## 1. Read-only qualification and pre-change backup

On ben1, run the currently installed release's plan preflight and capture
read-only state. Verify the exact NFS mount with `findmnt`, loopback listeners,
service state, current tunnel ownership, and free bytes/inodes. Do not infer
these facts from an earlier run.

Create a pre-change backup using the **currently installed** backup script under
`with-control-plane-lock.sh`. This matters because the incoming backup script
requires the new key and reconciled receipt. Promote no change until the backup
checksum passes and `restore-test.sh` has restored that backup on ben4 under a
unique direct child of `/var/tmp/blazn-restore`.

**Hold point A:** pre-change backup and isolated restore evidence are complete;
ben1 health and both tunnel ownership records are unchanged.

## 2. Stage the exact release

Install the reviewed release at `/opt/blazn` without changing the active
service. Verify the checkout is the approved commit and contains migration
`003_workspaces.sql`. Run the repository's infrastructure, API, Go, generated
client, and isolated PostgreSQL checks on a non-live Linux host first.

Do not run Compose manually. Keep systemd as the sole restart owner. All ben1
build, secret, receipt, backup, stop, and start mutations use the same
`ben1-control-plane-mutation` wrapper and a recorded correlation ID.

## 3. Install and bind the invitation key

With the service still running, invoke exactly once through the mutation lock:

```sh
sudo /opt/blazn/infra/milestone-2/scripts/with-control-plane-lock.sh \
  workspace-secret <correlation-id> auto \
  /opt/blazn/infra/milestone-2/scripts/upgrade-live-v2-to-workspace.sh
```

The upgrade is retryable. It generates 32 random bytes as 64 lowercase hex
characters, installs a root-owned mode-`0444` named file inside a mode-`0700`
root directory, emits no key material, and records only its digest in the
separate upgrade receipt. A pre-existing unreceipted key or mismatched partial
state fails closed.

Next, under separate lock acquisitions, run `build-control-api.sh`, review its
source-digest/image receipt, and run `update-receipt-config.sh`. The main receipt
must now bind:

- the API source digest, including all migration files;
- the immutable API image and image ID;
- the full control-plane configuration digest; and
- the digest of `workspace-invitation-hmac-v1`.

Run `preflight.sh --deploy` through the lock. It must reject an absent,
mis-owned, malformed, rotated, or receipt-mismatched key.

**Hold point B:** both receipts validate, no secret was printed, and Compose
configuration shows the named key mounted only into the long-running `api`
service—not PostgreSQL, MinIO, migration, bootstrap, or tool containers.

## 4. Serialized migration and restart

Use `systemctl restart blazn-control-plane.service`. The unit synchronously
acquires the same mutation lock, rebuilds/verifies the receipt-bound image,
runs `api-migrate`, runs the restart-idempotent bootstrap and object initializer,
waits for API health, and verifies every API container image ID. Do not issue a
second restart or Compose command while this operation is active.

Verify migration `003` is recorded exactly once. Using the restricted runtime
database role, prove Workspace DML succeeds only through the intended grants and
that schema changes, role changes, and cross-workspace access fail. Re-run API
health, authentication, restart, and revocation checks through public TLS.

**Hold point C:** migration, least-privilege, image, health, ngrok, and HomeAI
tunnel evidence all pass. Otherwise enter rollback and do not create workspace
data.

## 5. Two-user Workspace acceptance

Use separate machines or OS accounts so each user has a distinct CLI credential
store. Authenticate each existing identity with `blazn auth login`; record only
redacted user/device identifiers.

As user A:

```sh
blazn workspace create poc-company --slug poc-company --request-id ws-create-<unique>
blazn workspace list
blazn workspace members poc-company
```

Create the one-time invitation and stream it directly to user B's stdin. The
token must never appear in argv or a retained file:

```sh
blazn --output json workspace invite poc-company --role member \
  --expires-in 15m --request-id ws-invite-<unique> \
  | jq -er .inviteToken \
  | ssh <user-b-qualified-host> \
      'blazn workspace join --invite-stdin --request-id ws-join-<unique>'
```

Then verify:

1. Repeating the same create/invite/join request IDs returns the same result and
   does not create duplicates; changing a bound request with the same ID fails.
2. User B can list/get the joined workspace and cannot edit it, invite users,
   or manage membership while role `member` is active.
3. A second workspace owned by user A is indistinguishable from not found to
   user B across get, members, invitations, cursors, and event streams.
4. Start `blazn workspace watch poc-company` as user B. User A removes user B
   with the current membership version. The stream emits revocation/ends, a
   reconnect is denied, and all later workspace operations are denied.
5. A fresh invitation can re-add user B. Reusing a consumed, revoked, expired,
   or altered token fails; tokens never appear in invitation lists, audit rows,
   server logs, ngrok logs, or retained shell history.
6. Restart the service and repeat list/get/watch to prove persistence and
   restart-safe migration/bootstrap behavior.

Do not remove or demote the immutable initial owner during qualification.

## 6. Post-change backup and isolated restore

Create a new backup with the incoming `backup.sh` under the mutation lock. Its
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

## 7. Rollback boundary

Stop on any receipt, key, migration, role, API, tunnel, backup, or cross-workspace
failure. Preserve logs and incomplete staging evidence without secret values.

- Do not rotate or delete `workspace-invitation-hmac-v1`; doing so invalidates
  outstanding deterministic invitations. Preserve it across application
  rollback and restore.
- Prefer application rollback to the last reviewed image when migration `003`
  is additive and the prior API ignores its tables. Rebuild the exact prior
  source, reconcile its image/config receipt under the lock, verify the main
  receipt still binds the same key, then let systemd restart it.
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

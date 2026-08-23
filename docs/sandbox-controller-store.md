# Sandbox controller database authority

Migration `013_sandbox_controller_queue.sql` adds the durable, database-only
boundary used by a future Sandbox reconciler. It does not start a controller,
call Kubernetes, install manifests, or enable an API route.

`blazn_sandbox_controller` is a no-login capability role in the control-plane
bootstrap. A future service login may inherit it, but the role itself has no
table privileges. Its only authority is the reviewed security-definer surface:

- claim one due operation with `FOR UPDATE SKIP LOCKED` and a 5–300 second
  database-clock lease;
- renew, bind a backend, retry, or complete only while presenting the exact
  worker identity and unexpired random lease token;
- enqueue bounded expiry-stop operations; and
- receive typed scalar and array fields required to reconcile a Sandbox.

Claims include immutable template, image, placement, resource, repository, and
source-commit identity. They do not include credentials, template JSON, secret
references, environment variables, raw Kubernetes objects, or caller-controlled
JSON. Repository identities cannot contain URL userinfo, query parameters, or
fragments, so they cannot smuggle inline credentials. Error events and terminal results are assembled by PostgreSQL from bounded
reason codes, safe messages, request UUIDs, artifact UUIDs, and warning codes.

One partial unique index permits at most one `pending` or `running` operation per
Sandbox. Every claim has a new fencing token. An expired lease may be reclaimed,
but the old holder can no longer renew, bind, retry, or complete it. The fifth
failed attempt becomes `recovery_required`; it is not silently retried forever.
Backend UID, resource version, and admission ID are compared as one exact tuple
before terminal mutation. Successful create requires all three identities;
successful stop/delete requires cleanup, artifact export, grant revocation, and
backend destruction before PostgreSQL accepts the receipt.

Expiry scanning uses the database clock and row locks with `SKIP LOCKED`.
Enqueue, Sandbox desired-state mutation, the operation, queue row, and monotonic
Sandbox event commit atomically. Per-Sandbox row locking serializes controller
event sequence allocation with the API's existing operation transactions.

Run the focused pinned PostgreSQL 17 and Node 22.19 proof with:

```sh
make test-sandbox-controller-postgres
```

The harness proves exclusive claims, renewal and stale-lease fencing, bounded
retry/recovery, exact backend/admission completion, partial uniqueness,
concurrent expiry enqueue, monotonic events, and denial of direct controller
table reads and writes. It uses a disposable Docker network and database only;
it performs no live-cluster mutation.

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
source-commit identity. Claim v3 also returns the requesting user and the exact
artifact name, path, media type, and required flag arrays in canonical name
order, plus the complete persisted Sandbox, Pod, and Workload observation.
The original, v2 claim/bind/complete functions are no longer executable by the
controller role, preventing an older worker from silently accepting incomplete
evidence. Claims do not include credentials, template JSON, secret
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

The adapter receipt contract and `sandbox_workload_admissions` represent admission as the exact Kueue Workload
API version, namespace, name, UID, resource version, and admitted ClusterQueue.
It also binds the Workload's immutable Sandbox owner reference (API version,
kind, name, and UID) and the frozen workspace and Sandbox correlation labels to
the same receipt identity. A Workload owned by or labelled for another Sandbox
or workspace is rejected even when its own Workload tuple is otherwise valid.
The owner reference must be controller-owned, and the observed Workload must
carry both an admitted status and an `Admitted=True` condition. The complete
tuple is canonicalized and SHA-256 bound independently by Go and PostgreSQL.
The scalar `admission_id` is the Workload UID; terminal completion also carries
a foreign-keyed admission digest. A requested LocalQueue name, name-only
identity, unadmitted Workload, or mutable observation cannot qualify a terminal
create receipt.

Migration `019_sandbox_admission_observation.sql` adds the frozen Pod API
version, kind, namespace, name, UID, and resource version and binds the complete
Sandbox → Pod → Workload observation with the canonical
`sandbox-admission-observation-v1` SHA-256 digest. The SQL digest reconstructs
the Sandbox identity from the persisted backend tuple and includes the existing
canonical Workload digest, matching the Go and TypeScript implementations.
Pod fields and the observation digest are nullable only as one all-or-none
group. No legacy row is backfilled: a Workload-only row can be claimed only so
the fenced controller can mark its operation `recovery_required`, and it cannot
be upgraded in place, authorize Kubernetes mutation, or complete successfully.
Fresh bind replay succeeds only when every persisted Sandbox, Pod, Workload,
lease, operation, and digest field is exact. The controller passes the persisted
observation into admission, deletion, finalization, and absence checks after a
restart; in-memory observation or cleanup caches are never authoritative.

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

## Controller secret mounting

`BLAZN_SANDBOX_CONTROLLER_DATABASE_URL_FILE` intentionally retains the stricter
private-file contract reviewed with this authority boundary: a regular file
owned by the controller UID, mode `0600`, and one hard link. A Kubernetes Secret
projection is a symlink and is not accepted directly. The eventual deployment
must use an init container to copy the database URL from its read-only Secret
projection into a controller-owned `emptyDir`, set the final owner and `0600`
mode, and mount only that copied file into the controller container. Runtime
code must not weaken this check to accommodate projection symlinks.

The in-cluster Kubernetes token has a different lifecycle and is read fresh
from its bounded projected volume for every API request so rotation works. The
Kubernetes CA must be presented as a stable regular file (the deployment may
use the same init-copy pattern). No deployment, ServiceAccount, Role, or
RoleBinding is added by the client-wiring slice itself.

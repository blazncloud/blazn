# Node Contract Freeze

**Contract:** `nodes/v1alpha1`
**Scope:** enrollment, standalone installation, node identity, capabilities,
heartbeats, Kubernetes binding, local-model supply, and safe lifecycle operations

## Narrow POC gate

The milestone passes when:

1. A clean Ubuntu 26.04 AMD64 host with only the released `blazn` binary can
   authenticate and run `blazn node install`; Blazn installs and owns every
   other dependency, joins only as a worker, starts its node service, and
   reports `active`, `verified`, and `agentEligible:true`.
2. One existing Linux worker and the `mac-mini-3-agent` Lima worker can be
   adopted by exact Kubernetes Node UID without reinstalling or deleting their
   runtime.
3. List/get/watch, label, pause/resume, cordon/uncordon, identity rotation,
   bounded drain, repair, update-status, and remove operate through auditable,
   idempotent Operations. Remove revokes Blazn eligibility and identity; it
   never deletes a control-plane member, Mac VM, or user data.
4. A node advertises versioned CPU/memory/disk/platform/sandbox capabilities
   and at least one local OpenAI-compatible model route. A workspace-authorized
   agent can route one inference request to that model without exposing the
   model endpoint outside the authenticated node tunnel.
5. Interrupted install resumes from its receipt; failed install restores only
   receipt-owned changes. Enrollment/join credentials are single-use,
   short-lived, hashed at rest, absent from argv/logs/receipts, and unusable
   after consumption.

The fresh host may be a named disposable VM. `ben4` remains an adoption and
isolated-test lane unless a separately approved reset/restoration plan names it
as the fresh host.

## CLI contract

```text
blazn node enrollment create --workspace WORKSPACE --name NAME --platform linux|macos [--mode fresh|adopt]
blazn node install [--workspace WORKSPACE] [--enrollment-stdin] [--dry-run]
blazn node list [--workspace WORKSPACE]
blazn node get NODE
blazn node watch NODE [--cursor CURSOR]
blazn node label NODE KEY=VALUE --expected-version VERSION
blazn node cordon|uncordon|pause|resume|rotate-identity|update NODE --expected-version VERSION
blazn node drain NODE --deadline DURATION --expected-version VERSION
blazn node remove NODE --expected-version VERSION --yes
blazn node doctor
blazn node repair
blazn node uninstall --yes
blazn node daemon
```

All resource mutations require an idempotency key (generated once and retained
across CLI transport retries) plus the expected Node version. Human and
deterministic JSON output are supported. Watch uses SSE/JSONL with `Last-Event-ID`.
Stable exit categories reuse the root CLI: `0` success, `1` internal, `2`
invalid input, `3` auth/identity, `4` policy/permission, `5` not found, `6`
version/state/idempotency conflict, `7` compatibility/unavailable, `8`
deadline/queue timeout, and `9` partial operation or recovery required.

Operation bodies are discriminated by type. Cordon/uncordon/drain/remove carry
the exact cluster ID, expected Kubernetes Node UID, and resourceVersion. Drain
also carries the workspace owner selector and a 60–3600 second deadline; the
server derives the exact eligible Pod set and accepts no arbitrary selector.
Remove requires `confirm:true` and `preserveHostData:true`. Label keys are
restricted to the `blazn.dev/` namespace. Pause/resume/rotate/repair accept an
empty parameter object, and update accepts only a semantic target version.

## API and persistence

[`nodes.openapi.json`](../packages/contracts/nodes.openapi.json) is the HTTP
source of truth. [`004_nodes.sql`](../services/control-api/migrations/004_nodes.sql)
is the persistence source of truth. The migration role creates the tables;
infrastructure must provision `blazn_node_broker` as a no-login-or-dedicated,
non-superuser, no-create-role/database, no-replication, no-bypass-RLS role
before migration and grant it only the join-issuance operations in the migration.
The broker has read-only access to the Node, enrollment, and signed-plan rows
needed for verification, plus select/insert/update on join issuances. It has no
membership, user, credential, capability, operation, or general Node mutation
privilege.
The runtime role can read only issuance identity/binding/timing columns and
update `consumed_at` plus `joined_node_uid`; ciphertext, credential hash, and
encryption key ID remain broker-only. PostgreSQL composite keys bind receipt
key ID/fingerprint/generation to the same Node identity row. A Node's current
identity foreign key includes status `active`, so revocation must atomically
clear eligibility or rotate the binding.
Migration `004_nodes.sql` must not be applied live until the separate Node
infrastructure PR has created an authenticated `blazn_node_broker` login,
root-owned database URL and encryption/HMAC key files, granted database CONNECT
and schema USAGE, and passed its positive/negative privilege test. The migration
fails closed if that prerequisite role is absent.

Operator authorization occurs before idempotency replay. The same
`(principal, operation, idempotency-key)` cannot create different resources;
every replay verifies workspace, target, request digest, and current authority.
Node lifecycle state and Kubernetes mutations serialize by locking the Node row
and rechecking expected version, bound cluster ID, Node UID, resourceVersion,
and operation state inside the transaction.
API digests use the rendered `sha256:<64-lowercase-hex>` form; PostgreSQL
`char(64)` digest columns store only the lowercase payload. The persistence
adapter must validate the prefix and length before stripping it and must restore
the prefix on output. No unvalidated string conversion is permitted.

## Enrollment and identity

An operator creates a short-lived enrollment with an explicit idempotency key.
The token is derived with the root-owned
`/etc/blazn/node-broker/secrets/enrollment-hmac-v1` key over
`blazn-node-enrollment-v1\n<workspace-id>\n<enrollment-id>\n<principal-id>\n<idempotency-key>`.
The token is unpadded base64url of the 32-byte HMAC-SHA256 output. Only its
SHA-256 hash and key ID are stored, so an authorized identical retry
reconstructs the same secret without persisting plaintext.
The authenticated enrollment-creation response also returns the active plan
signing key ID, raw Ed25519 public key, and SHA-256 fingerprint. The CLI verifies
their consistency and pins that tuple in its origin/workspace credential state
before the public token exchange. A plan is never trusted merely because it
carries its own key ID.

The enrolling binary generates an Ed25519 key locally and sends its raw public
key plus a stable machine fingerprint over TLS. The raw 32-byte public key uses
unpadded base64url. Its fingerprint is lowercase SHA-256 over those raw bytes;
PostgreSQL stores the 64 hex characters and API/plan values render
`sha256:<hex>`. The server persists both and binds the
enrollment to that machine and returns a signed
[`NodeInstallPlan`](../packages/contracts/nodes/node-install-plan.schema.json).

The Node Bootstrap Broker is a separate least-privilege process. It may issue
one short-lived worker join credential only after verifying the plan signature,
workspace/operator approval, unconsumed enrollment, machine fingerprint, node
public key, expected cluster, and worker-only profile. It cannot issue
control-plane/datastore membership. The credential is delivered in a protected
response body, held only in memory or a root-owned `0600` install journal, and
is hashed in PostgreSQL. For response-loss replay it is also encrypted with
AES-256-GCM under `/etc/blazn/node-broker/secrets/join-credential-v1`; the row
stores key ID and `12-byte nonce || ciphertext || 16-byte tag`, request digest,
and idempotency key. AAD is UTF-8
`blazn-node-join-credential-v1\n<workspace-id>\n<enrollment-id>\n<plan-id>\n<node-id>\n<issuance-id>\n<idempotency-key>\n<request-digest>`.
An
authorized identical retry decrypts the same unconsumed, unexpired credential.
After join, the node calls the consume endpoint; the control API atomically
marks issuance/enrollment consumed and binds cluster ID, Node name, UID, and
resourceVersion before enabling work.

Plan canonicalization is RFC 8785 JSON with `digest` and `signature` omitted.
`digest` is `sha256:` plus its lowercase SHA-256; `signature` is unpadded
base64url Ed25519 over the UTF-8 bytes
`blazn-node-install-plan-v1\n<digest>`. The CLI pins an approved signing-key ID
and public key before trusting any plan field, including source URLs or rollback
targets.
`VerifyNodeInstallPlan` recomputes this digest, verifies Ed25519 against the
pinned key ID, requires `issuedAt <= now < expiresAt`, and compares workspace,
enrollment, node, hostname, machine fingerprint, node public-key fingerprint,
platform, architecture, and idempotency key to trusted local input before any
plan source, mutation, or rollback field is consumed.
Verification also takes a locally configured trusted install profile. The
profile—not the signed plan—defines exact allowed download/registry hosts and
mutation roots for `ubuntu-26.04-amd64-worker/v1`,
`existing-linux-worker-adopt/v1`, or `macos-lima-worker-adopt/v1`. URL host must
equal the component `sourceHost`; redirects are revalidated. Targets may never
be `/`, contain `..`, escape the profile roots, or traverse a symlink. Mutation
kind/action/payload must match the schema's discriminated rules. Package and
image mutations name a signed component; repository/registry origin, version,
OCI reference, and digest must match that component and the local profile.
Components declare one source class. `https` requires an approved source host
and URL; packages/images additionally bind their repository or registry.
`current_binary` means the already-authenticated running `blazn` binary
installed by Milestone 1, and `embedded` means a digest-pinned service/config
asset compiled into that binary. Neither class permits a URL, removing any
bootstrap dependency on a private GitHub release or unreserved hostname.
Ubuntu/existing-Linux profiles require systemd, Linux image platform, the
profile architecture, `blazn-node:blazn-node`, and approved apt/snap inputs;
they reject launchd/brew. The macOS/Lima profile requires launchd, ARM64 Linux
images, `root:wheel`, approved brew inputs, and rejects systemd/apt/snap.
Fresh Linux creates/adopts the receipted `blazn-node` group and non-login user
before assigning `/var/lib/blazn`; the service may not start against a
root-only unwritable tree. The macOS profile embeds a digest-pinned Lima binding
configuration naming the exact existing VM/worker and requires
`lima_worker_binding` evidence before eligibility.

The long-running service uses a renewable node identity, never the user's
access/refresh token. Rotation overlaps old/new identities only for a bounded
window; a heartbeat signed by the new generation activates it and revokes the
old generation. Revocation closes streams and rejects heartbeats immediately.

## Standalone install and receipt ownership

`blazn node install` is the only user-facing setup surface. It may prompt for
`sudo`; it must never instruct the user to install Kubernetes, containerd,
packages, certificates, or services manually. It performs preflight, obtains
the signed plan, validates exact versions/checksums/platform/disk/network,
records a write-ahead journal, installs pinned dependencies, joins as a tainted
worker, installs the same binary as a root-owned service, verifies identity and
capabilities, then removes the bootstrap taint only after activation.

The plan and [`NodeInstallReceipt`](../packages/contracts/nodes/node-install-receipt.schema.json)
enumerate every package, file, directory, unit, certificate, image, label,
taint, firewall change, and pre-existing value. Each mutation is staged,
fsynced, compare-and-set, and receipt-owned. Repair/update/uninstall acquire a
host lifecycle lock, reconcile an exact journal generation, and refuse
ambiguous ownership. Uninstall preserves unrelated Kubernetes/runtime/user
state and emits `RECOVERY_REQUIRED` rather than broad cleanup.
Every pre-existing value has protected receipt-local rollback material: a
content/version/snapshot locator, digest, mode, UID, and GID. `restore_prior`
is invalid without it. Receipt digest is RFC 8785 JSON with `digest` and
`signature` omitted; the node identity signs
`blazn-node-install-receipt-v1\n<digest>` with Ed25519, and the server verifies
that signature before accepting the completed installation. Verification
requires the trusted active Node identity key ID, generation, public-key
fingerprint, and public key to exactly match the receipt; a generic keyring
lookup is insufficient.
Rollback locators are opaque single-segment `receipt-backup://<id>` values and
are resolved beneath the signed platform-specific backup root only after
no-symlink path validation. Linux uses `/var/lib/blazn/install-backups/<id>`;
macOS uses `/Library/Application Support/Blazn/install-backups/<id>` and never
relies on the `/var` symlink.

## Capabilities and local models

Capability snapshots are immutable, versioned, digest-bound, and accepted only
from the active node identity with a strictly increasing heartbeat sequence.
They include:

- Host OS/architecture/capacity and worker OS/architecture/allocatable capacity
  separately. A Mac host reports macOS/ARM64 while its Lima worker reports
  Linux/ARM64 and Kubernetes allocatable values.
- Kubernetes cluster/Node UID/resourceVersion, labels, limits, health, sandbox
  backends, and RuntimeClasses.
- Local model routes: stable route ID, display name, exact model identifier,
  protocol, loopback or authenticated-tunnel endpoint, context/output limits,
  capabilities, health, concurrency, and data boundary.

A local model such as DeepSeek V4 Flash or Qwen 3.8 remains bound to the node.
The control plane publishes metadata and health, never a raw LAN endpoint or
credential. The Proxy policy may select it only through an authenticated node
tunnel, workspace permission, model capability match, and `local` data boundary.

## Heartbeats and offline state

The daemon signs each heartbeat with node ID, identity generation, boot ID,
strictly increasing sequence, timestamp, and capability digest. The server
rejects replay, generation mismatch, excessive clock skew, and capability
payloads that fail schema or secret-key validation. `offline` is derived after
the advertised grace period; it does not revoke identity automatically.
Reconnect reconciles desired state before the node becomes agent-eligible.
The proof is unpadded base64url Ed25519 over RFC 8785 canonical JSON of the
request body prefixed by `blazn-node-heartbeat-v1\n`; join issuance uses the
same rule with prefix `blazn-node-join-v1\n`.
Capability digest is exactly `sha256:` plus lowercase SHA-256 over
`blazn-node-capability-v1\n` followed by RFC 8785 canonical JSON of the complete
capability. Heartbeat verification recomputes it before checking the signed
request and never trusts the supplied digest alone.

## Kubernetes and lifecycle safety

- Adoption requires exact cluster ID, Node name, UID, and resourceVersion.
- A control-plane/datastore member is never eligible for Blazn drain/remove.
- Cordon/uncordon compare the bound UID/resourceVersion and touch only the
  Blazn-managed unschedulable state/taints.
- Drain lists the exact candidate Pods first, excludes DaemonSets/static Pods,
  respects disruption budgets and deadline, and never widens selectors.
- `pause` prevents new Blazn work; `drain` moves only Blazn-owned workloads;
  `remove` revokes identity and binding after workloads are gone or returns a
  structured partial result.
- Shared-cluster changes use the fenced cluster mutation lock and one operator.

Every lifecycle operation ends with a signed
[`NodeOperationReceipt`](../packages/contracts/nodes/node-operation-receipt.schema.json)
containing before/after Kubernetes binding, ordered exact actions, terminal
outcome, and residues. Ambiguous cleanup is `partial` or `recovery_required`,
never ordinary success.
Its digest is `sha256:` plus lowercase SHA-256 over RFC 8785 canonical JSON with
`digest` and `signature` omitted. The active node identity signs the UTF-8 bytes
`blazn-node-operation-receipt-v1\n<digest>` with Ed25519; verification binds the
signing key ID, node ID, workspace, operation ID/type, and expected Node version
before the receipt may authorize any state or audit conclusion.
Executed host actions use a `node_identity` signer bound to the exact active
identity generation. Pre-dispatch failure, cancellation, unreachable daemon, or
revoked-node recovery may use a `control_plane` signer bound to the pinned
control-plane receipt key; that path cannot claim applied/restored host actions
or ordinary success. Thus every terminal Operation has signed evidence even
when the node cannot produce it.

## Acceptance evidence

- Signed-plan valid/wrong-signer/expired/tampered/platform mismatch tests.
- Enrollment replay, concurrent exchange, join issuance, response-loss retry,
  and credential redaction tests.
- Install fault injection after every owned mutation; reboot/SIGKILL resume;
  repair/update/uninstall and pre-existing-runtime refusal/adoption tests.
- Heartbeat replay/generation/skew/offline/reconnect/capability tests.
- Fake Kubernetes UID/resourceVersion/control-plane/drain/finalizer tests.
- Fresh Ubuntu AMD64 install E2E, existing Linux adoption, macOS/Lima adoption,
  identity rotation, bounded drain, exact rollback/residue proof.
- Local Qwen/DeepSeek capability publication and one authenticated company
  agent inference through the node tunnel, with no raw endpoint/secret leakage.

## PR split

1. Contract, migration, schemas, generated client.
2. Control-plane Node API and persistence.
3. Enrollment/bootstrap broker.
4. CLI install/state machine and Linux system service.
5. Node daemon, heartbeat, capability and local-model reporting.
6. Kubernetes binding/lifecycle adapter and Mac/Lima adoption.
7. Serialized qualification/evidence.

## Deferred hardening

Windows nodes, multiple Kubernetes distributions, production HA broker/API,
hardware attestation/TPM keys, automatic OS patching, arbitrary LAN model
exposure, generalized GPU scheduling, and automated control-plane restoration
are outside this POC.

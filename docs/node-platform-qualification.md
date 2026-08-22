# Node platform qualification

This runbook qualifies the frozen `nodes/v1alpha1` platform contract without
treating a unit-test pass as native-platform evidence. It produces two
independent evidence runs:

1. a fresh Ubuntu 26.04 AMD64 worker in a correlation-owned disposable LXD
   guest and disposable Kubernetes cluster; and
2. the existing `mac-mini-3` host with the exact `mac-mini-3-agent` Lima worker.

The harness lives in [`infra/node/qualification`](../infra/node/qualification).
It is deliberately separate from product code and deployment workflows. It
does not initialize LXD, create a cluster, create a macOS account, edit
sudoers, load a launch daemon, create or modify a Lima VM, or prepare target
credentials. Those are platform mutations and need their own reviewed plan and
authorization. No command in this runbook may target `ben4` itself, the
`frontro-agent-worker` VM, or the shared Frontro MicroK8s cluster.

## Pass boundary

The Node platform qualification passes only when both run manifests finalize
with every required gate. A missing, skipped, inferred, or manually asserted
gate fails closed.

The fresh-Linux run requires:

- canonical `https://github.com/blazncloud/blazn.git` source remote, exact
  source HEAD/tree/status digest, released binary version/digest, pinned LXD
  image fingerprint, correlation ID, target, and complete before inventory;
- a clean Ubuntu 26.04 AMD64 guest that is not the LXD host, with only the
  released Blazn binary and explicitly recorded authentication bootstrap;
- the structured `lxd-create` result with the accepted approval-input digest,
  image fingerprint, target, and bounded CPU/memory/root-disk/process limits;
- install, same-request idempotent install, no-op repair, signed-plan-expired
  root observation, expired-plan repair denial, expired-plan uninstall with
  managed-runtime removal, and a new-request reinstall;
- service account UID/GID, systemd `User`, live process UID, and successful
  `sudo -n /usr/local/bin/blazn node-root-observe` executed as `blazn-node`;
- install and cleanup crash/resume cases from a clean correlation-owned LXD
  snapshot. A crash is accepted only when the root WAL is at the exact reviewed
  checkpoint and `/proc/PID/comm` is exactly `blazn`;
- exact Kubernetes Node UID/resourceVersion evidence, a stale-resourceVersion
  JSON Patch rejected atomically without changing resourceVersion, and an
  unschedulable Blazn bootstrap/quarantine `NoSchedule` taint with zero ordinary
  workloads on the node;
- uninstall and LXD deletion followed by zero correlation-labelled Kubernetes
  resources, no Node, no guest, and byte-for-byte stable normalized protected
  service/container inventory, including HomeAI, plus exact target path,
  package, snap, account, unit, firewall, and network inventory; and
- no enrollment token, join credential, access/refresh token, Authorization
  header, or private key in evidence.

The native-Mac run requires the corresponding adoption, lifecycle,
Kubernetes, crash/cleanup, and residue gates. It additionally proves the host
is the approved ARM64 `mac-mini-3`, the exact Lima VM is running, and its worker
name is `mac-mini-3-agent`. It never deletes or recreates the Mac, its user, the
Lima VM, or user data. Native crash qualification must use a separately
approved host recovery checkpoint; the LXD snapshot commands are not a Mac
rollback mechanism.

## Immutable inputs and approvals

Use a unique DNS-safe ID, for example `nodequal-20260822-a1`. The LXD guest must
be named `blazn-q-20260822-a1` or use that exact prefix. Every mutating action
requires all of the following:

```bash
export BLAZN_QUALIFICATION_CORRELATION_ID=nodequal-20260822-a1
export BLAZN_QUALIFICATION_TARGET=blazn-q-20260822-a1
export BLAZN_QUALIFICATION_PROFILE=lxd-ubuntu-26.04
export BLAZN_QUALIFICATION_MODE=mutate
export BLAZN_QUALIFICATION_APPROVED_HEAD="$(git rev-parse HEAD)"
export BLAZN_QUALIFICATION_LOCK_FILE=/var/lock/blazn-qualification/node-lifecycle-blazn-q-20260822-a1.lock
source infra/node/qualification/lib/common.sh
qual_export_lock_identity
input_digest=$(qual_approval_input_digest ACTION)
export BLAZN_QUALIFICATION_APPROVAL="APPROVE:${BLAZN_QUALIFICATION_CORRELATION_ID}:${BLAZN_QUALIFICATION_TARGET}:ACTION:${input_digest}"
```

`ACTION` is not reusable. Examples are `lxd-create`,
`lifecycle-install`, `lifecycle-repair`, `kubernetes-stale-cas`, and
`crash-install-binding`. The lock must already be a regular, non-symlink,
root-owned, mode `0600`, `0640`, or `0644` file under `/var/lock/blazn-qualification` or
`/run/lock/blazn-qualification`. The harness takes it with nonblocking
`flock`. The external coordinator must also hold the fenced
`live-cluster-mutation` and `node-lifecycle/<node>` locks for their complete
foreground operations. The local lock is not a replacement for those locks.

An approval is invalid after a source HEAD, target, correlation, action,
released binary digest, LXD image fingerprint, cluster identity, Node UID, or
Node resourceVersion change. Never put credentials in these variables, shell
arguments, approval strings, evidence metadata, or command logs.
The digest is canonical JSON over the action, source HEAD, target/profile,
binary and image digests, guest CPU/memory/root-disk/process limits, snapshot,
cluster/context identity, Node UID/resourceVersion, trusted profile path and
content digest, workspace, request IDs, machine fingerprint, operator identity,
expected native hostname, Lima VM, crash timeout, plan expiry, and lock
path/device/inode/owner/mode identity. Set every applicable input and create the
root-owned lock before calculating it; changing any bound value requires a new
approval.
Snapshot approvals additionally bind the canonical clean target-state digest
and immutable snapshot identity digest.

## Safe local checks

These commands perform static or dry-run work only:

```bash
infra/node/qualification/tests/test-static.sh

export BLAZN_QUALIFICATION_CORRELATION_ID=nodequal-dryrun01
export BLAZN_QUALIFICATION_TARGET=blazn-q-dryrun01
export BLAZN_QUALIFICATION_PROFILE=lxd-ubuntu-26.04
export BLAZN_QUALIFICATION_LXD_IMAGE_FINGERPRINT=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
infra/node/qualification/lxd-disposable.sh plan
infra/node/qualification/lifecycle.sh plan
```

The placeholder fingerprint is acceptable only for printing a plan. Resolve
and review the actual Ubuntu 26.04 image fingerprint before approval. A
read-only LXD-host preflight is:

```bash
export BLAZN_QUALIFICATION_MODE=dry-run
infra/node/qualification/preflight.sh | jq .
```

It requires an already configured LXD client and performs only `lxc info`.
It does not initialize LXD. `native-mac-preflight.sh` similarly uses read-only
`uname`, `scutil`, `id`, `launchctl print`, `limactl list`, and a no-input
observer attempt. It must execute on `mac-mini-3` itself:

```bash
export BLAZN_QUALIFICATION_CORRELATION_ID=nodequal-macpre01
export BLAZN_QUALIFICATION_TARGET=mac-mini-3
export BLAZN_QUALIFICATION_PROFILE=native-mac
export BLAZN_QUALIFICATION_EXPECTED_HOSTNAME=mac-mini-3
export BLAZN_QUALIFICATION_LIMA_VM=frontro-agent-worker
export BLAZN_QUALIFICATION_KUBE_NODE=mac-mini-3-agent
infra/node/qualification/native-mac-preflight.sh | jq .
```

The VM name in this native command is observed/adopted only. The LXD guard's
ban on the shared `frontro-agent-worker` target remains in force: it may never
be supplied as `BLAZN_QUALIFICATION_TARGET` or deleted/recreated.
The current VM's shared-cluster binding is read-only preflight evidence, not an
authorization to run native lifecycle qualification. Install/adopt, repair,
uninstall, crash, or Kubernetes write gates remain blocked until a reviewed
disposable-cluster Lima binding and native recovery plan exist. The harness
rejects the current shared cluster ID and API origin before invoking enroll.

## Evidence initialization

Create evidence outside the repository on protected storage. Do not use `/tmp`
for retained evidence.

```bash
evidence_root=/protected/blazn-evidence/${BLAZN_QUALIFICATION_CORRELATION_ID}/fresh-linux
infra/node/qualification/evidence.py init \
  --output "$evidence_root" \
  --repo "$PWD" \
  --scope fresh-linux \
  --binary /protected/releases/blazn
```

The initializer refuses an existing directory and noncanonical origin. It
records HEAD, tree, exact status digest, and binary digest/version. Each raw
command writes separate stdout and stderr files beneath
`$evidence_root/artifacts`. Record the observed exit and expected exit without
editing either artifact:

```bash
infra/node/qualification/evidence.py record \
  --output "$evidence_root" \
  --step source-provenance \
  --stdout "$evidence_root/artifacts/source.stdout" \
  --stderr "$evidence_root/artifacts/source.stderr" \
  --exit-code 0 --expect-exit 0 \
  --metadata '{"command":"reviewed source capture v1"}'
```

The recorder creates immutable, unique step IDs, hashes both streams, binds
the current correlation, and parses stdout as one JSON document against the
gate-specific semantic contract. A successful exit or a generic
`{"status":"passed"}` assertion is insufficient. The verifier repeats semantic
validation so replacing an artifact and updating only its descriptor cannot
manufacture a passed gate. It cross-binds source/inventory HEAD and tree,
correlation/target, pre/post protected and target inventories, complete receipt
identity/digests/signature metadata, and the released binary digest. Mutating
gate artifacts persist the exact accepted approval-input digest. It also rejects
common credential markers. Redact at the
producer before writing an artifact; never edit an artifact after recording.
The verifier detects changed size or digest.

After install succeeds but before recording any receipt, run
`lifecycle.sh identity-observe` and record its output as
`node-identity-trust`. The receipt-authorized root observer supplies the
nonsecret raw Ed25519 public key, fingerprint, signing-key ID, identity
generation, enrollment/Node/workspace IDs, and control-plane-origin digest.
The recorder refuses every receipt until that authoritative tuple is pinned.
`--receipt-public-key` is only an optional equality assertion and cannot select
trust. The recorder recomputes each canonical receipt digest and verifies the
domain-separated signature with OpenSSL. Every active or removed receipt must
also match the observed identity generation, signer, and Node. A removed
receipt is a distinct signed document; reusing the active signature fails.

## Baseline and protected-workload invariants

On the LXD host, explicitly name every existing service and container that must
remain stable. Include all HomeAI Compose containers and their owning service.
The script reads only stable unit/container fields and never reads environment,
mount contents, Docker labels, command lines, or secret values.

```bash
export BLAZN_QUALIFICATION_PROTECTED_UNITS=homeai.service,another-existing.service
export BLAZN_QUALIFICATION_PROTECTED_CONTAINERS=homeai-api,homeai-db
infra/node/qualification/capture-inventory.sh before \
  >"$evidence_root/artifacts/baseline.json"
```

Missing `systemctl`/Docker access, an unloaded unit, or an unobservable
container fails rather than producing an incomplete baseline. Capture `after`
only after uninstall, Kubernetes cleanup, and guest deletion, then run:

```bash
infra/node/qualification/compare-invariants.py \
  "$evidence_root/artifacts/baseline.json" \
  "$evidence_root/artifacts/after.json"
```

For native Mac, record the exact pre/post launchd service, Lima VM/worker,
Blazn-owned paths, and unrelated application invariants with reviewed read-only
commands. Do not claim the Linux Docker inventory script as Mac evidence.

After the explicitly approved binary, authentication, and trusted-profile
staging is complete—but before `node enroll`—capture the disposable guest's
lifecycle baseline:

```bash
infra/node/qualification/capture-target-state.sh before \
  >"$evidence_root/artifacts/target-before.json"
```

This records only normalized metadata or SHA-256 digests for the released
binary, Blazn roots, MicroK8s snap roots, packages/snaps, service account/unit,
firewall, and network state. It does not emit file contents. Capture `after`
after receipt-owned uninstall and before deleting the guest. Exact comparison
proves lifecycle cleanup returned to the staged baseline; later guest deletion
is not used as a substitute for uninstall evidence.

## Disposable Ubuntu 26.04 LXD lifecycle

Before creation, independently resolve the immutable fingerprint for the
reviewed Ubuntu 26.04 AMD64 image. Do not approve a mutable alias. Creation is
one action:

```bash
export BLAZN_QUALIFICATION_LXD_IMAGE_FINGERPRINT=<64-lowercase-hex>
export BLAZN_QUALIFICATION_LXD_CPU=4
export BLAZN_QUALIFICATION_LXD_MEMORY=8GiB
export BLAZN_QUALIFICATION_LXD_ROOT_DISK=32GiB
export BLAZN_QUALIFICATION_LXD_PROCESSES=1024
source infra/node/qualification/lib/common.sh
qual_validate_lxd_limits
qual_export_lock_identity
input_digest=$(qual_approval_input_digest lxd-create)
export BLAZN_QUALIFICATION_APPROVAL="APPROVE:${BLAZN_QUALIFICATION_CORRELATION_ID}:${BLAZN_QUALIFICATION_TARGET}:lxd-create:${input_digest}"
infra/node/qualification/lxd-disposable.sh create
```

The script refuses an existing instance, launches an unprivileged guest with
an integer CPU limit from 1 through 8, integer memory limit from 1GiB through
16GiB, root disk from 16GiB through 64GiB, and process limit from 256 through
2048, and writes correlation/purpose instance properties. All limits and the
immutable image fingerprint are approval-bound.
`inspect` is read-only. Snapshot, restore, and delete each need a distinct
approval, for example:

```bash
export BLAZN_QUALIFICATION_SNAPSHOT=checkpoint-clean-ubuntu
export BLAZN_QUALIFICATION_CLEAN_TARGET_STATE_SHA256="sha256:$(jq -cS '.state' "$evidence_root/artifacts/target-before.json" | sha256sum | awk '{print $1}')"
source infra/node/qualification/lib/common.sh
qual_export_lock_identity
input_digest=$(qual_approval_input_digest lxd-snapshot)
export BLAZN_QUALIFICATION_APPROVAL="APPROVE:${BLAZN_QUALIFICATION_CORRELATION_ID}:${BLAZN_QUALIFICATION_TARGET}:lxd-snapshot:${input_digest}"
infra/node/qualification/lxd-disposable.sh snapshot
```

The script will restore/delete only a guest whose name and instance property
match the correlation. It never performs a wildcard operation.
Create, snapshot, restore, and delete emit structured JSON containing the exact
accepted approval-input digest. Before snapshot, set
`BLAZN_QUALIFICATION_CLEAN_TARGET_STATE_SHA256` to the SHA-256 of canonical
`.state` from `target-before.json`. The snapshot action re-observes the guest
immediately and refuses drift. Its structured identity binds the LXD instance
UUID, snapshot creation time/name, expanded config digest, and clean target
content digest. Use the returned `identityDigest` as
`BLAZN_QUALIFICATION_SNAPSHOT_IDENTITY_SHA256` for restore/crash approvals.
Deleting and recreating an instance with the same name/config cannot reuse it.

After cloud-init/network readiness, record `/etc/os-release`, `uname`, disks,
interfaces, routes, package/runtime absence, accounts, units, and Kubernetes
absence. Copy only the exact released binary whose SHA-256 is approved. Any
copy, account/sudo preparation, authentication bootstrap, or profile placement
is a mutation outside this harness and needs separate authorization and
evidence. It must not be smuggled into LXD creation.

## Install and lifecycle sequence

Set nonsecret exact inputs. The operator UID/GID is the ordinary authenticated
guest user; root is explicitly refused. The profile is the already reviewed
local trusted-profile file. Authentication remains in its protected native
store and is never an environment variable.

```bash
export BLAZN_QUALIFICATION_BLAZN_BIN=/usr/local/bin/blazn
export BLAZN_QUALIFICATION_BINARY_SHA256=sha256:<64-lowercase-hex>
export BLAZN_QUALIFICATION_OPERATOR_UID=1000
export BLAZN_QUALIFICATION_OPERATOR_GID=1000
export BLAZN_QUALIFICATION_WORKSPACE=<workspace-id>
export BLAZN_QUALIFICATION_REQUEST_ID=<unique-install-request-id>
export BLAZN_QUALIFICATION_REINSTALL_REQUEST_ID=<different-reinstall-request-id>
export BLAZN_QUALIFICATION_MACHINE_FINGERPRINT=sha256:<64-lowercase-hex>
export BLAZN_QUALIFICATION_INSTALL_PROFILE=/etc/blazn/node/profiles/qualification.json
export BLAZN_QUALIFICATION_INSTALL_PROFILE_SHA256=sha256:<64-lowercase-hex>
export BLAZN_QUALIFICATION_CLUSTER_ID=<disposable-cluster-id>
export BLAZN_QUALIFICATION_CLUSTER_ORIGIN=https://<disposable-api-host>:<port>
```

Run each mutating command with its corresponding
`lifecycle-ACTION` approval:

```bash
infra/node/qualification/lifecycle.sh install
infra/node/qualification/lifecycle.sh idempotent-install
infra/node/qualification/lifecycle.sh repair
```

After each success, record the receipt JSON and independently inspect it. An
active install/reinstall receipt must have `state=active`, `currentStage=complete`,
zero residues, all mutations applied, exact plan/binary/service digests, and
the expected node identity. Repair must be a no-op with unchanged owned state.
Run `observe-target.sh` to bind Ubuntu version, service UID/GID, systemd user,
live process UID, and the daemon account's no-input root observation.

To test expiry, bind `BLAZN_QUALIFICATION_PLAN_EXPIRES_AT` to the exact locally
persisted `exchange.plan.expiresAt`, then wait until that signed time; do not
change the host clock. `expired-observe` is read-only and must succeed. Then
separately approve `expired-repair-denied` and `expired-uninstall`. The repair
attempt must return the exact underlying `install plan is not active at trusted
current time` verification error. The harness persists the plan expiry, digest,
and signature in the denial evidence; a generic `node_failed` wrapper fails.
Uninstall must produce a verified `removed`
receipt with zero residues. A recovery-required receipt fails the gate even if
later manual cleanup appears successful.

For reinstall, use the distinct request ID and a currently signed plan. Reusing
the consumed enrollment or silently extending the expired plan fails.

## Crash checkpoints

Production code exposes no qualification-only fault-injection environment
variable. Crash evidence therefore uses an external `SIGKILL` only in the
disposable guest. Restore the approved clean snapshot before every case. The
read-only observer is available for diagnosis:

```bash
infra/node/qualification/crash-checkpoint.sh observe install binding
```

The actual fault and recovery run is integrated so one process holds the
lifecycle lock throughout snapshot restore, mutation, kill, and recovery. It
recomputes the approval-bound immutable snapshot identity, restores that exact
snapshot while holding the lock, rechecks the instance correlation marker, and
persists the instance UUID, snapshot creation/name, config and clean-content
digests, identity digest, and `restoredUnderLifecycleLock=true`
in the crash evidence. A separate kill
process cannot acquire or bypass that lock. Set the exact snapshot, one-use
approval, and bounded poll timeout, then invoke the action name itself:

```bash
export BLAZN_QUALIFICATION_SNAPSHOT=checkpoint-clean-ubuntu
export BLAZN_QUALIFICATION_CLEAN_TARGET_STATE_SHA256=sha256:<target-state-digest>
export BLAZN_QUALIFICATION_SNAPSHOT_IDENTITY_SHA256=sha256:<snapshot-identity-digest>
export BLAZN_QUALIFICATION_CRASH_TIMEOUT_SECONDS=300
source infra/node/qualification/lib/common.sh
qual_export_lock_identity
input_digest=$(qual_approval_input_digest crash-install-binding)
export BLAZN_QUALIFICATION_APPROVAL="APPROVE:${BLAZN_QUALIFICATION_CORRELATION_ID}:${BLAZN_QUALIFICATION_TARGET}:crash-install-binding:${input_digest}"
infra/node/qualification/lifecycle.sh crash-install-binding
```

The integrated runner starts the ordinary non-root command, records its exact
in-guest PID in a correlation-bounded mode-`0600` temporary file, polls the
root WAL, verifies `/proc/PID/comm`, sends `SIGKILL` only at the approved
checkpoint, removes the exact PID file, and runs `node recover` (install) or
the exact uninstall again (cleanup) before releasing the lock. The allowlist
contains exact install checkpoints `join_intent`, `join`, `binding`,
`broker_consume`, `broker_consumed`, `verify`, and `receipt`, plus cleanup
checkpoints `cleanup_pending`, `cleanup_support_removed`, and
`cleanup_local_state_removed`. The runner refuses PID 1, nonnumeric PIDs,
non-Blazn `/proc` names, a mismatched WAL, a mismatched guest correlation, or
Mac/native targets. Inspect the integrated recovery output and prove its final
signed receipt and zero ambiguous residues; do not run an unrecorded extra
recovery command. A process exit alone is not crash-resume evidence.

Expired-plan binding never reads the service-owned `0600` runtime file as the
ordinary operator. The existing receipt-authorized
`sudo -n /usr/local/bin/blazn node-root-observe` surface returns only the public
signed-plan ID, expiry, digest, and signature alongside its narrow capability
observation; it exposes no token, key, origin, mutation, or rollback material.

## Kubernetes checks

Kubernetes qualification is permitted only against a disposable context whose
name does not contain `frontro`, `microk8s`, `ben1`, or `shared`. The
`blazn-qualification` namespace must already have annotation
`blazn.dev/qualification-correlation=<correlation>`. Creating that namespace or
cluster is a separate approved mutation.

The harness also hard-refuses the frozen Frontro cluster ID
`frontro-microk8s-8f109e68-f1bf-40e5-8482-c97d10997dc2` and API origin
`https://192.168.0.108:16443`, even if a context is renamed. It requires the
exact disposable API origin and `kube-system` namespace UID in addition to the
context and correlation marker.

```bash
export BLAZN_QUALIFICATION_KUBE_CONTEXT=<disposable-context>
export BLAZN_QUALIFICATION_CLUSTER_ID=<disposable-cluster-id>
export BLAZN_QUALIFICATION_CLUSTER_ORIGIN=https://<disposable-api-host>:<port>
export BLAZN_QUALIFICATION_KUBE_SYSTEM_UID=<exact-kube-system-namespace-uid>
export BLAZN_QUALIFICATION_KUBE_NODE=<exact-node-name>
export BLAZN_QUALIFICATION_EXPECTED_NODE_UID=<exact-uid>
export BLAZN_QUALIFICATION_EXPECTED_RESOURCE_VERSION=<exact-rv>
infra/node/qualification/kubernetes-checks.sh inspect
infra/node/qualification/kubernetes-checks.sh quarantine
```

`inspect` and `quarantine` are read-only. Refresh and reapprove the exact
resourceVersion before every mutation. `stale-cas` deliberately sends a JSON
Patch with a false resourceVersion `test` followed by a same-value write. It is
still an API write attempt and therefore requires the
`kubernetes-stale-cas` approval and cluster lock. Passing requires either a
structured Kubernetes `Status` rejection with reason `Invalid`, status code
`422`, and a message identifying the JSON Patch test/resourceVersion failure,
or kubectl's exact `Error from server (Invalid):` rendering of that same test
failure. The harness normalizes either form into structured evidence. RBAC,
authentication, transport, admission, or unrelated validation failures do not
pass. A reread must also prove both UID and resourceVersion unchanged.

The quarantine check requires `spec.unschedulable=true`, an exact
`blazn.dev/bootstrap` or `blazn.dev/quarantine` taint with effect `NoSchedule`,
and no non-DaemonSet/non-static pod on the node. It does not accept a similarly
named label as taint evidence.

## Cleanup and finalization

Uninstall before deleting a guest so receipt-owned cleanup can be observed.
Delete only after the removed receipt, Kubernetes Node removal, and
correlation-resource scan pass. Then capture the protected `after` inventory
and run:

```bash
infra/node/qualification/zero-residue.sh \
  "$evidence_root/artifacts/baseline.json" \
  "$evidence_root/artifacts/after.json" \
  "$evidence_root/artifacts/target-before.json" \
  "$evidence_root/artifacts/target-after.json"
```

The scan fetches Kubernetes object names only, not Secret bodies. It refuses a
remaining guest, Node, correlation-labelled object, or protected invariant
change. Also record explicit filesystem/account/unit/runtime residue checks
from the guest before deletion; deletion by itself cannot prove uninstall
ownership.

After recording every required step, finalize and immediately verify:

```bash
infra/node/qualification/evidence.py finalize --output "$evidence_root"
infra/node/qualification/evidence.py verify --output "$evidence_root"
```

Repeat with `--scope native-mac` in a separate directory. The JSON Schema is
necessary but not sufficient: the verifier also checks artifact digests,
unique step IDs, correlation binding, every scope-specific gate, and passed
terminal state. Independent review must compare the raw evidence with the
approved source HEAD, release digest, plan/receipt signatures, target inventory,
cluster inventory, and this pass boundary before the Node milestone is marked
qualified.

## Required authorization not supplied by this runbook

The following remain blocked until explicitly authorized and prepared:

- the exact remote host/operator for LXD, an already initialized LXD service,
  the reviewed Ubuntu 26.04 AMD64 image fingerprint, root-owned lifecycle lock,
  guest limits, and guest create/snapshot/restore/delete approvals;
- released Blazn asset source, SHA-256, signature verification evidence, and
  authorization to copy it into the guest;
- target authentication bootstrap, workspace, trusted install profile,
  machine fingerprint, request IDs, ordinary operator UID/GID, and each Node
  install/repair/expired/uninstall/reinstall approval;
- a disposable Kubernetes cluster/context, exact correlation namespace marker,
  cluster identity, Node name/UID/resourceVersion, fenced mutation locks, and
  each Kubernetes write approval;
- each exact LXD crash checkpoint/PID approval and snapshot restore approval;
- the `mac-mini-3` user/host identity, exact existing Lima VM binding, native
  checkpoint/recovery plan, and any DSCL, sudoers, launchd, Lima, or node
  lifecycle mutation approval; and
- the authoritative protected workload/HomeAI unit and container inventory and
  read-only Docker/systemd access needed to prove it unchanged.

Until those inputs exist, only static tests, dry-run plans, and read-only
preflights are authorized evidence. A missing authorization is not a waiver or
a skipped gate.

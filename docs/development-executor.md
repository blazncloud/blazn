# Development build executor

The Development controller is a separate least-privilege process. It claims a
durable build lease, renews that lease while work is active, invokes the pinned
BuildKit client over mTLS, persists content-addressed evidence Artifacts, and
then enters the existing fenced finalizer. It never runs with the Management
API database role.

Build the controller image from `services/control-api`:

```sh
docker build -f Dockerfile.development-controller -t blazn-development-controller:candidate .
```

The image copies `buildctl` from the exact BuildKit image currently qualified
by the POC cluster. It runs as the unprivileged `node` user and does not mount a
container-engine socket.

## Secret and process contract

Mount a read-only secret directory at
`/etc/blazn/development-controller/secrets` (or set the absolute
`BLAZN_DEVELOPMENT_SECRETS_ROOT`). It must contain:

- `database-url`: connection string for `blazn_development_controller`.
- `buildkit-address`: exact `tcp://host:port` address.
- `buildkit-server-name`: DNS identity checked by TLS.
- `buildkit-builder-id`: stable UUID for this qualified builder installation.
- `registry-authority`: exact DNS authority of the authorized Development OCI
  registry. The resolver refuses every other authority and follows no redirects.
- `buildkit-ca.pem`, `buildkit-cert.pem`, and `buildkit-key.pem`: a dedicated,
  short-lived BuildKit client identity. Do not reuse the BuildKit server key.

`BLAZN_DEVELOPMENT_EVIDENCE_COMMAND` names one absolute, root-owned executable
mounted read-only into the container. The controller sends one bounded JSON
request on stdin. It includes the exact source/build identity, BuildKit result
digest, hashed (not plaintext) BuildKit log identity, and controller-derived
stable Artifact IDs. The command returns one bounded JSON response containing:

- verified live `nodeId` and `sandboxId` placement;
- the complete `blazn.dev/build/v1alpha1` terminal document; and
- one base64 UTF-8 JSON payload for every required typed evidence Artifact.

The controller recomputes every Artifact digest, requires canonical JSON (which
also rejects duplicate-key shadowing), rejects missing or substituted roles,
scans all evidence recursively for credential-like material, and atomically
stores the complete evidence set plus terminal receipt through lease-fenced SQL
authority. It also binds success to the observed BuildKit index digest and exact
qualified builder identity. The semantic finalizer runs in that same database
transaction.
The collector cannot directly write controller tables.

A nonzero `buildctl` exit is retried as an operational failure. Because no
immutable image index was observed, it is never converted into a terminal
Build document or passed to the evidence collector.

`BLAZN_DEVELOPMENT_EXECUTION_TIMEOUT_SECONDS` bounds the complete BuildKit and
collector operation (one hour by default, two hours maximum). Cancellation
sends TERM and escalates to KILL after five seconds before the queue continues.

The pinned image now includes the first concrete collector stage. It durably
creates or re-adopts one ordinary Sandbox lifecycle record for every committed
platform/test pair, freezes the exact argv and timeout, and waits for the
controller's complete Sandbox/Pod admission observation. It cannot receive the
Build lease token, mutate controller evidence tables, or execute a shell.
After that real lifecycle observation, the collector persists a lease-fenced
`preparing -> ready` transition. Execution authority accepts only persisted
`ready` or idempotent `running` rows; complete admission evidence alone cannot
skip the transition.

The controller resolves the exact BuildKit index bytes at the allowlisted
registry and supplies the two architecture child digests to the collector. The
collector atomically records a Development-only immutable binding for that
index, both child digests, workspace, build, and active execution generation.
Each Development test run references the matching platform binding before its
ordinary Sandbox is prepared; replay must be byte-for-byte exact and stale or
cross-workspace workers are fenced. Ordinary Sandboxes remain foreign-key bound
to their published template images and those fields are never rewritten to the
candidate image. Candidate projection at Sandbox claim time re-locks the exact
active Development job generation; expired, reclaimed, completed, or unrelated
create operations are quarantined, while a bound delete cleanup retains the
ordinary Sandbox image tuple.

The command transport must independently verify
the frozen Sandbox and Pod UIDs, resolve the scheduled Kubernetes Node to an
active verified Blazn Node, bound output to digests and byte counts, and drive
stop/delete cleanup. Complete terminal success additionally requires real
two-platform security, lifecycle, refresh, registry inspection, reproducibility,
and cleanup evidence. Missing stages remain operational retries; no successful
or terminal receipt is synthesized.

## Current cluster prerequisite

The live `frontro-buildkit/buildkit-mtls` Secret contains a server-only
certificate (`TLS Web Server Authentication`). A dedicated client certificate
signed by the BuildKit CA is therefore a deployment prerequisite. Copying the
server private key into this controller is prohibited.

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

The collector is intentionally not a generic shell hook. Its implementation
must use the existing Sandbox create/ready/exec/artifact/stop/delete lifecycle,
run the committed argv tests without a shell, resolve the observed Kubernetes
Node to an active verified Blazn Node, inspect the two immutable image children,
and bind a distinct reproducibility baseline. Until that concrete collector is
installed, the controller retries and emits an `operational-failure` event on
each fifth unsuccessful attempt; it never fabricates a successful or terminal
Sandbox receipt.

## Current cluster prerequisite

The live `frontro-buildkit/buildkit-mtls` Secret contains a server-only
certificate (`TLS Web Server Authentication`). A dedicated client certificate
signed by the BuildKit CA is therefore a deployment prerequisite. Copying the
server private key into this controller is prohibited.

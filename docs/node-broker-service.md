# Node Bootstrap Broker service

The broker process exposes only `POST /v1/node-service/join-credentials`. It
rejects bearer credentials and authenticates the exact request body with the
enrolled Node public key. Before issuance it independently verifies the stored
plan signature and all workspace, enrollment, plan, Node, machine, public-key,
cluster, worker-only, lifecycle, trust, expiry, and request-digest bindings.

The public API reaches this process through a reverse route.
`BLAZN_NODE_BROKER_LOOPBACK` accepts only `enabled` or `disabled`. By default
the route targets the loopback sidecar at `http://127.0.0.1:8081`.

A deployment whose API cannot share a host with the root-owned worker issuer
(for example a Kubernetes API pod under the restricted Pod Security profile) may
set `BLAZN_NODE_BROKER_URL` to a private IPv4 `http://` origin with an explicit
port. A non-loopback origin requires `BLAZN_NODE_BROKER_CALLER_KEY_FILE`, a
43–128 character base64url key sent as `X-Blazn-Broker-Caller` on every call.
The broker binds to `NODE_BROKER_BIND` (default `127.0.0.1`); any non-loopback
bind requires the same key file, and whenever a key is configured every broker
route, including health and join observation, rejects a missing or different
key in constant time. Restrict the broker port with a host firewall to the API's
source addresses; the key is defense in depth, not a substitute. Only the closed JSON body, singular idempotency key, and
singular Node proof are forwarded—never bearer credentials, cookies, proxy
headers, or client address headers—and redirects are not followed. One overall
deadline and both payload directions are bounded. Rate limiting occurs only at
the public API, after proof/body validation, using its trusted-proxy-derived
remote identity; the loopback broker has no shared-peer bucket.

Enabled API startup is deliberately degraded rather than cyclic: the API may
listen, but health returns broker-unavailable until the sidecar is live.
Compose starts the sidecar after the API container is started (not healthy),
then waits for API health before public/ngrok readiness. Before listening, the
broker probes the helper protocol/MicroK8s readiness, database with a statement
timeout, and AES key. Every later broker/API health request repeats all three
probes, redacts failures, and observes recovery without an API restart.

The database connection must use `blazn_node_broker`. That role can read only
`nodes`, `node_enrollments`, and `node_install_plans`, and can mutate only
`node_join_issuances`. Issuance serializes by Node, stores only a SHA-256 hash
and AES-256-GCM ciphertext, and reconstructs an identical response after a
response-loss retry. The key ID is `node-join-credential/v1`; the AAD is the
frozen value in `docs/node-contract.md`. Provider credentials are compensated
if database persistence fails.

The `WorkerCredentialIssuer` boundary is deliberately narrow. The broker first
commits a deterministic issuance intent whose UUID is also the provider handle.
A provider must treat issue and revoke as idempotent for that handle, honor the
supplied `AbortSignal`, and never continue issuing after its deadline. Pending
or `revoke_required` intents are revoked before retry, so a crash or ambiguous
database commit cannot leave an untracked live credential. A provider must
prove the expected cluster is healthy and return a short-lived, worker-only
credential for the expected Node name and bootstrap taint. Arbitrary commands,
admin kubeconfigs, and user or management tokens are outside this interface.
The broker refuses to start without an injected provider.

This PR does not claim an end-to-end Node join. A real MicroK8s provider and
platform adapter must implement this interface and be live-qualified before the
Node join milestone is complete.

## Crash and retry phases

| Durable state | Meaning | Retry behavior |
| --- | --- | --- |
| `pending` | Intent committed; no provider owner yet | Claim a bounded lease, revoke the deterministic handle, then issue |
| `issuing` with live lease | Another request owns provider work | Poll for the encrypted issuance without holding a database connection |
| `issuing` with expired lease | Owner crashed or exceeded its deadline | Reclaim, idempotently revoke the same handle, then issue |
| `revoke_required` | Persistence or provider work failed and compensation was inconclusive | Revoke the persisted handle before any new issue |
| `revoked` | Compensation completed | Reuse the same intent/handle for a clean retry |
| `completed` | Encrypted issuance committed | Decrypt and replay; a completed intent without its issuance fails closed |

An ambiguous commit is resolved by rereading the issuance before compensation.
If the row exists, it is replayed and the live provider credential is retained;
otherwise the intent remains recoverable until deterministic revocation works.

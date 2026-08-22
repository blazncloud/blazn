# Node Bootstrap Broker service

The broker process exposes only `POST /v1/node-service/join-credentials`. It
rejects bearer credentials and authenticates the exact request body with the
enrolled Node public key. Before issuance it independently verifies the stored
plan signature and all workspace, enrollment, plan, Node, machine, public-key,
cluster, worker-only, lifecycle, trust, expiry, and request-digest bindings.

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

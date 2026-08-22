# MicroK8s worker credential issuer

The Node Bootstrap Broker never receives a kubeconfig, root access, or an
arbitrary command surface. Its `WorkerCredentialIssuer` connects to a
root-controlled Unix socket and may request only two operations: issue one
short-lived worker join credential for a deterministic issuance UUID, or revoke
that exact provider handle.

The compiled production socket is `/run/blazn/microk8s-worker-issuer.sock`.
Durable issuance/revocation state is rooted at the Linux-only privileged path
`/var/lib/blazn-node-root/microk8s-worker-issuer`, never beneath the
daemon-owned `/var/lib/blazn` tree. macOS hosts do not run this helper directly;
their digest-bound Lima worker uses the same Linux root-state contract inside
the VM.
The receipt installer must create `/run/blazn` as root and the broker primary
group with mode `0750`; the helper creates the socket as root and that group
with mode `0660`. Tests inject the provider object directly; production has no
socket-path environment override.

The helper accepts only the configured cluster ID, a DNS-safe expected node
name, `workerOnly: true`, the exact
`blazn.dev/bootstrap=pending:NoSchedule` taint, and a TTL from 1–300 seconds. A
separate 32-byte HMAC key derives the exact 32-character MicroK8s bootstrap
token from the protocol domain, issuance UUID, cluster, expected name, taint,
TTL, and worker-only constant. The key is not the broker AES key or enrollment
HMAC key.

MicroK8s v1.35.6 revisions 9072 (AMD64) and 9075 (ARM64) ship identical
`add_token.py` and `microk8s-add-node.wrapper` implementations. The reviewed
command is fixed to:

```text
/snap/bin/microk8s.add-node --token <derived-32-hex> --token-ttl <1..300> --format json
```

Before claiming `clusterHealthy`, it runs only the additional fixed readiness
probe `/snap/bin/microk8s.status --wait-ready --timeout 5`.

The helper parses a closed JSON response, requires the same token plus server
certificate check in every cluster-agent URL, and returns a base64url-encoded
closed credential payload. The stock file may contain permanent `<32hex>`
administrator tokens or expiring `<32hex>|<10-digit-epoch>` tokens. Broker
issuance requires exactly one bounded expiring form. Revocation never rewrites
the file: it durably changes only that token's fixed-width expiry in place to
the canonical past value `0000000001`, preserving concurrent appends and all
unrelated permanent or expiring tokens.
The credentials directory must be root-owned, mode `0770`, and owned by the
configured MicroK8s administrator group. The token file is root-owned, owned
by that exact group, single-linked, and exactly mode `0660`.

Durable root-only intent files are written before invoking MicroK8s. A retry of
`pending` or `revoke_required` state first revokes the deterministic token. An
`issued` retry returns the byte-identical credential; revoke is idempotent.
Evidence and errors never include token, token-check, URLs, credential, HMAC
key, or command output.

## Honest live boundary

Stock MicroK8s bootstrap tokens are not cryptographically bound to the joining
node name, and MicroK8s exposes no token-revoke CLI. The helper records the
expected name and bootstrap taint, but live join remains blocked until the
follow-up platform hook verifies the new Kubernetes Node name/UID, applies and
observes the bootstrap taint before eligibility, and quarantines unexpected
joins. Broker proof, signed-plan binding, a single deterministic token, and the
short TTL reduce exposure; they do not replace that hook.

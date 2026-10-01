# Node infrastructure

This directory holds the reviewed inputs and host tooling for Blazn nodes:

- `templates/node-install-plan-template-v1.json` and
  `templates/current-binary-digests.json`: the signed install-plan template and
  the release binary digests it pins. A release candidate rotates both (plus
  any embedded release tag) before the release is published.
- `node-install-plan-template.schema.json` and
  `microk8s-worker-issuer-receipt.schema.json`: the schemas those materials and
  the issuer receipt must satisfy.
- `scripts/install-worker-issuer.sh`, `scripts/upgrade-worker-issuer-observation.sh`
  and `scripts/rollback-worker-issuer.sh`: lifecycle of the root-owned MicroK8s
  worker issuer described below.
- `scripts/verify-database.sh`: proves the `blazn_node_broker` database role's
  exact privilege matrix before and after the Node migrations.
- `postgres/ensure-controller-roles.sh`: creates the controller roles in the
  disposable Postgres used by `scripts/test-node-postgres.sh`.

The hosted API signs install plans with the key and template from its
Kubernetes secret; the node broker runs beside the issuer and reaches it only
through the issuer socket.

## MicroK8s worker issuer boundary

`install-worker-issuer.sh` provisions the root helper, distinct HMAC
generation, closed config, broker socket group, systemd/tmpfiles policy,
recovery inventory, and crash-resumable receipt. The node broker container
receives only its database URL, AES join key, and fixed Unix socket—never the
issuer HMAC key, Docker socket, kubeconfig, or MicroK8s directory.
`upgrade-worker-issuer-observation.sh` transactionally moves an existing
blocked receipt to the observation-enforced binary while preserving recovery
material. Live use still requires the disposable-node qualification in
`docs/microk8s-worker-issuer-infra-runbook.md`.

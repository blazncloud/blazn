# MicroK8s worker issuer infrastructure runbook

This installs only the credential issuer boundary. It does not issue a
credential or join a Node. Join consumption now fails closed unless the issuer
observes the exact expected Kubernetes Node name, UID, resource version, single
bootstrap taint, and worker-only role. Never apply live qualification to `ben1`
or shared MicroK8s.

Before installation, hold the serialized control-plane lock and record merged
source/helper/ownership-receipt digests, broker UID and pre-provisioned empty
dedicated `blazn-node-broker` account/group/GID (primary group only,
`/nonexistent`, `/usr/sbin/nologin`, no UID/GID collision), MicroK8s v1.35.6
revision 9072 or 9075, `microk8s` GID, and zero active bootstrap issuances.
Confirm HomeAI is outside the Compose project and rollback targets. Refuse
managed paths without the issuer receipt, links, or missing operator
correlation.

Read-only preflight includes `snap info microk8s`, the `current` revision,
both group records, helper SHA-256, and `docker compose --profile node-broker
config`. After normal prebackup and restore qualification, install under the
lock with root-owned `BLAZN_ISSUER_BINARY_SOURCE`, its exact
`BLAZN_ISSUER_BINARY_SHA256`, and the receipt-bound broker UID.

Verify the receipt is complete and says `liveJoinBlocked: false`, the service is
active, and `/run/blazn` plus its socket have the receipted owner/GID/modes.
The broker profile must expose no Docker socket, kubeconfig, MicroK8s directory,
host network, added capability, or issuer HMAC key. Do not enable live issuance
until the exact release has passed the disposable-node qualification below.

Qualification must prove that consumption fails for an absent Node, a wrong
name/UID/resource version, a missing or duplicate bootstrap taint, and any
control-plane/master label or taint. It must then prove that the exact
quarantined worker observation permits consumption, activation removes the
bootstrap taint, and the active worker becomes heartbeat-verified and eligible.

An existing complete receipt with `liveJoinBlocked: true` is not accepted by
the installer. Stop the Node broker sidecar first. Under the same serialized
lock, run `upgrade-worker-issuer-observation.sh` with the reviewed
observation-enforced binary path and SHA-256. The upgrade stops the issuer,
atomically replaces only the receipt-owned binary, changes the gate and binary
digest, updates the exact main-receipt material binding, and restarts the
service. Its root-only recovery journal records the prior/result binary,
issuer-material, main-receipt, and unchanged recovery-inventory digests. Every
intermediate phase is resumable; any unrelated receipt, binary, or recovery
change fails closed.
Keep the broker stopped until the matching control API build is installed and
the issuer health/observation protocol plus the disposable-node canary pass.

Before accepting backup v4, copy the receipt-bound recovery key and issuer
receipt into the separately protected Node recovery inventory as
`microk8s-issuer-hmac-v1` and `microk8s-worker-issuer.json`. Run
`verify-backup-metadata.sh`; it recomputes the stable issuer material digest,
checks the main ownership and backup metadata bindings, and matches the HMAC
digest without placing key material in ordinary backup evidence.

Rollback first removes the broker sidecar, then runs
`rollback-worker-issuer.sh` under the same lock. It refuses changed artifacts,
removes only receipt-owned active files, and retains the root-only recovery
key, inventory, receipt, and empty broker group for audited disposal.

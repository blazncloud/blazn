# Frontro hosting overlay

The Kubernetes objects that host Blazn on the Frontro MicroK8s cluster, as
they are deployed. Operating procedures are in
[`docs/delivery/hosting-runbook.md`](../../docs/delivery/hosting-runbook.md).

| Path | Contents |
|---|---|
| `namespaces.yaml` | The `blazn-test` and `blazn-identity-dev` namespaces. |
| `blazn-test/` | Control API (`api-dev`), object store, registry, their ConfigMaps, ServiceAccounts, PVCs, NetworkPolicies, and the cert-manager Issuers and Certificates behind their TLS Secrets. `kubectl apply -k`. |
| `blazn-identity-dev/` | The Mailpit capture inbox used by qualification. The ZITADEL stack that also lived here was retired on 2026-10-01. |
| `edge/blazn-routes.yaml` | Reference copy of the Blazn routes in the shared Traefik gateway. Not applied from here. |
| `secrets.json` | Names, types and key names of the Secrets the manifests expect. No values. |
| `export.py` | Regenerates everything above from the live cluster; `--check` reports drift. |
| `deploy-api.sh` | Builds, migrates, publishes and rolls out the control API. |
| `node-broker.sh` | Restarts the node broker container on the issuer host. |
| `sender-domain/` | Prepared, not applied: DNS records, a DNS check and the steps for moving the sign-in email sender to `mail.blazn.frontro.com`. |
| `scripts/` | Helpers used by `deploy-api.sh` on the build and cluster hosts. |

The manifests are generated, not hand-written: `export.py` reads the live
objects, strips runtime-only fields, and never reads Secret values. Applying
them to the cluster they came from changes nothing:

```sh
kubectl diff -f infra/frontro/namespaces.yaml
kubectl diff -k infra/frontro/blazn-test
kubectl diff -k infra/frontro/blazn-identity-dev
```

A rebuild on a new cluster additionally needs the OpenBao roles and secrets and
the Kubernetes Secrets listed in `secrets.json`, the `frontro-hostpath-srv`
storage class, the `blazn_test` database and roles, and the gateway routes.

The sandbox controller and its admission boundary are not part of this
overlay; `infra/agent-sandbox` installs them through journaled transactions.

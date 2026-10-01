# Hosting runbook: Blazn on the Frontro cluster

This is how the hosted development deployment of Blazn is put together and
operated. The manifests it refers to are in [`infra/frontro`](../../infra/frontro).
No secret values appear here or in the repository: only names and locations.

## What runs where

| Component | Where | Notes |
|---|---|---|
| Control API (`api-dev`) | namespace `blazn-test`, pinned to `ben5` | Serves `https://api.blazn.frontro.com`. Pod has the API, an OpenBao agent (init and renewal), and an nginx sidecar on 8081 that adds the trusted-proxy header. |
| Agent Run controller | container `agent-run-controller` in the `api-dev` pod | Executes queued Agent Runs: starts the harness in the Run's Sandbox, relays messages, calls the model. Same image as the API. See [`docs/agent-run-controller.md`](../agent-run-controller.md). |
| Database | `frontro-db-1` (192.168.0.105), Postgres 17, database `blazn_test` | Roles: `blazn_runtime` (API), `blazn_migration` (migrations), `blazn_node_broker` (broker), `blazn_agent_run_controller` (Agent Run controller), plus the sandbox controller's role. TLS with `verify-full`. |
| Object store | `objectstore` (MinIO) in `blazn-test`, PVC `storage-data` | Bucket `blazn-test`. Used by the API and the sandbox controller. |
| Image registry | `registry` in `blazn-test`, PVC `registry-data` | `registry.blazn-test.internal`; holds the control API image. |
| Edge | Traefik `moments-direct-gateway` in `moments-direct` (192.168.0.204) | Shared Frontro gateway. The Blazn routes are recorded in `infra/frontro/edge/blazn-routes.yaml`. Only `/v1/`, `/activate` and `/healthz` are routed to the API. |
| Email | Resend (sender `no-reply@mail.blazn.benpelo.com`) | Sign-in codes for the reserved capture domain go to `identity-mail` (Mailpit) in `blazn-identity-dev` instead. |
| Worker issuer | `ben1`, systemd `blazn-microk8s-worker-issuer` | Root-owned; issues MicroK8s worker join credentials over `/run/blazn/microk8s-worker-issuer.sock`. |
| Node broker | `ben1`, Docker container `blazn-node-broker` | Listens on `192.168.0.100:18081`. The API reaches it through NetworkPolicy `api-dev-node-broker-egress` and `BLAZN_NODE_BROKER_URL`. |
| Sandbox controller | namespace `blazn-poc-system` | Installed and upgraded by the journaled tooling in `infra/agent-sandbox`; sandboxes run in `blazn-poc-sandboxes`. |
| Mail capture | `identity-mail` (Mailpit) in namespace `blazn-identity-dev` | Receives sign-in codes for the reserved capture domain so qualification can sign in. Nothing else runs in that namespace. |

Blazn nodes join the Frontro MicroK8s cluster (API VIP `192.168.0.108:16443`)
with the permanent taint `blazn.dev/sandbox-node=true:NoSchedule`.

## Secrets

Runtime secrets are rendered into pods by OpenBao agents
(`openbao.frontro-secrets.svc.cluster.local:8200`), authenticated with each
pod's Kubernetes service account.

| OpenBao path | Role | Used by |
|---|---|---|
| `apps/data/blazn/test/api` | `blazn-test-api` | API: database password, object-store keys, proxy key, invitation key, node-enrollment key |
| `apps/data/blazn/test/storage` | `blazn-test-storage` | Object store root credentials |
| `apps/data/blazn/test/registry` | `blazn-test-registry` | Registry push credential |
| `apps/data/blazn/test/migration` | `blazn-test-migration` | Migration role password |

Kubernetes Secrets created by hand are listed, by name and key only, in
`infra/frontro/secrets.json`. The two that carry Blazn-specific material:

- `api-dev-email`: the Resend API key and the sign-in code HMAC key.
- `api-dev-node`: the install-plan signing key, the plan template, the
  enrollment HMAC key, and the broker caller key.

- `api-dev-agent-run`: the Agent Run controller's database URL
  (`database-url`) and its model routes (`model-routes.json`). A route with a
  credential names a file; add that file as another key of this Secret.

The Agent Run controller's database role is created by the migrations without
a login. On the hosted database it was given `LOGIN`, a password, and
`pg_hba.conf` lines next to the other `blazn-test` entries.

The TLS Secrets `objectstore-tls`, `registry-tls` and `test-ca` are issued by
cert-manager from the Issuers and Certificates in `infra/frontro/blazn-test`.

On `ben1`, the broker's secrets are in `/etc/blazn/node-broker-frontro/secrets`
(root-only) and the issuer's in `/etc/blazn/microk8s-worker-issuer`.

## Check health

```sh
curl -fsS https://api.blazn.frontro.com/healthz
```

Expect `"status":"ok"`, `"database":"ok"` and `"objectStorage":"ok"`. A 503 with
`node_broker_unavailable` means the API cannot reach the broker: check the
container on `ben1`, then the issuer.

```sh
ssh ben1 'sudo docker ps --filter name=blazn-node-broker; systemctl is-active blazn-microk8s-worker-issuer'
```

Agent Runs: the controller logs one JSON line per event.

```sh
ssh ben1 'sudo microk8s kubectl -n blazn-test logs deploy/api-dev -c agent-run-controller --tail=50'
```

A Run that stays `queued` with `sandbox_node_unobserved` retries means the
Sandbox controller has not recorded the Sandbox's Node; check its logs for
"Agent placement observation failed". The Node must be an active Blazn node in
the Sandbox's workspace.

`infra/qualification/qualify-flows.py` exercises sign-up, sign-in, workspaces,
projects, invitations, roles and sign-out against the hosted API.

## Deploy the API

From a checkout of the commit to deploy:

```sh
infra/frontro/deploy-api.sh --migrate
```

This builds `services/control-api` on the build host, applies migrations with
the migration role, pushes the image to the in-cluster registry, sets the new
digest on `deployment/api-dev`, waits for the rollout and checks `/healthz`.
The deployment rolls: the new pod must pass its readiness check before the old
one is stopped (`maxSurge: 1`, `maxUnavailable: 0`), and the old pod keeps
serving for a few seconds after it is told to stop, so a deploy does not
interrupt requests. Both pods run for a short time; the namespace quota has
room for one extra pod. An Agent Run that the old pod was driving is claimed by
the new pod once its lease expires (about 90 seconds); recovery of a Run in
flight is not qualified yet (plan item M6.4).
Afterwards record the new digest:

```sh
KUBECTL="ssh ben1 sudo -n microk8s kubectl" infra/frontro/export.py
git add infra/frontro && git commit
```

Roll back by setting the previous digest (it is in the last commit of
`infra/frontro/blazn-test/deployment-api-dev.yaml`):

```sh
ssh ben1 'sudo microk8s kubectl -n blazn-test set image deployment/api-dev api=registry.blazn-test.internal/blazn/control-api@sha256:<previous>'
```

Migrations are forward-only. A rollback across a migration needs a restore of
`blazn_test`, so keep migrations backward-compatible with the previous image.

`deploy-api.sh` also builds the in-Sandbox harness (`blazn-agent`, for amd64
and arm64) into the image and sets the same digest on the
`agent-run-controller` container.

The namespace has the ResourceQuota `test-budget`. A new container must fit
inside it, or the rollout stalls with a quota error on the ReplicaSet.

After a rollout the API can answer `node_broker_unavailable` until the issuer's
first health probe has completed once: the cold probe takes longer than the
broker's timeout. Run the check under "Check health"; it clears on the second
attempt.

## Change other objects

Edit the manifest under `infra/frontro/<namespace>/`, review the server's view
of the change, then apply:

```sh
kubectl diff -k infra/frontro/blazn-test
kubectl apply -k infra/frontro/blazn-test
```

`infra/frontro/export.py --check` fails when the live cluster no longer matches
the repository. Run it before changing anything, so an out-of-band change is
noticed rather than overwritten.

## Node broker and issuer

The broker runs the same image as the API. After deploying an API change that
touches the broker, restart it on `ben1` with that image:

```sh
sudo infra/frontro/node-broker.sh blazn-test-control-api:<revision>
```

If the issuer and broker restart together, the broker can log
`microk8s_unavailable` although the issuer is healthy: restart the broker once
more. The broker's database pool does not recover from a Postgres restart
(error `57P01`): restart the broker.

The issuer accepts only the MicroK8s snap revisions pinned in
`internal/microk8sissuer` (9072 and 9075). A snap auto-refresh
to any other revision makes it answer `microk8s_unavailable` and blocks node
registration. Refreshes are therefore held on the cluster hosts:

```sh
sudo snap refresh --hold microk8s
```

To move to a new MicroK8s revision: add the revision and the SHA-256 of its
`add_token.py` and `microk8s-add-node.wrapper` to the issuer's pins, release
and install the new issuer with `infra/node/scripts/install-worker-issuer.sh`,
and only then unhold and refresh the snap on `ben1`.

## Releases of the CLI

1. Run the `release.yml` workflow in `candidate` mode with the new version.
2. Rotate `infra/node/templates/current-binary-digests.json` and
   `node-install-plan-template-v1.json` to the candidate's binary digests and
   merge that change.
3. Run `release.yml` in `publish` mode with the candidate run ID and its
   source commit.
4. Update the `node-install-plan-template-v1.json` key of Secret
   `api-dev-node` from `main` and restart `api-dev`, so issued plans pin the
   published binaries.

## Retired on 2026-10-01

- The earlier test deployment `api` (`https://blazn-test.frontro.com`) and the
  objects only it used. It had served nothing but its own health probes.
- The ZITADEL stack in `blazn-identity-dev` (API, login UI, Postgres). The API
  runs with no identity provider.

Their manifests are in git history (`infra/frontro` at commit `2e5215fb`).
ZITADEL's volumes (`database`, `bootstrap`), its cert-manager Certificates and
Issuers, and the TLS Secrets they produced were deleted afterwards, so its
data is gone and a restore would start from an empty identity store.

The Blazn routes for the retired services were removed from the shared
gateway's ConfigMap (`moments-direct/moments-direct-gateway`, key
`dynamic.yml`). The previous file is saved on `ben1` as
`~/blazn-backups/moments-direct-gateway-dynamic-before-20261001.yml`. The
gateway was restarted on 2026-10-01 to apply it (about 6 seconds of downtime);
the retired hosts and paths now answer 404.

**The gateway only reads that file at startup.** Traefik is configured with
`--providers.file.filename`, which does not notice a ConfigMap update (the
update arrives as a symlink swap, and Traefik watches for the file's own
name). A change to `dynamic.yml` therefore takes effect at the gateway's next
restart. The gateway is one replica with the `Recreate` strategy and also
serves Moments, The Archive and retailer-apply, so a restart is a few seconds
of downtime for all of them. Validate a changed file first by loading it in
the same Traefik image and listing `/api/http/routers`.

## Known hardening gaps

- The Frontro API server runs with `--authorization-mode=Node,RBAC` but
  without the `NodeRestriction` admission plugin, so a node's kubelet
  credential can modify other Node objects. Enabling it is a change to the
  shared control plane (`/var/snap/microk8s/current/args/kube-apiserver` on
  the control-plane host, then an API server restart) and needs its own
  review and a maintenance window.
  It also needs a product change first: `NodeRestriction` forbids a kubelet
  from changing its own Node's taints, and the node agent removes the
  `blazn.dev/bootstrap` taint and adds `blazn.dev/retired` with the kubelet
  credential. Those changes must move to the issuer before the plugin is
  enabled, or node activation and uninstall will fail.
- Sandboxes use the default container runtime. A hardened RuntimeClass is
  required before running untrusted code for other tenants.

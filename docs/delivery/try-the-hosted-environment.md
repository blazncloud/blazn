# Try the hosted development environment

This walks through Blazn's hosted development deployment at
`https://api.blazn.frontro.com`, using the published CLI. Every command here is
what the automated qualification runs. How the deployment is built and operated
is in the [hosting runbook](hosting-runbook.md); what is done and what is next
is in the [delivery plan](end-to-end-plan.md).

It is a development environment: data can be reset, and sandboxes are isolated
by orchestration only, so do not run untrusted code or use sensitive data.

## 1. Install the CLI

```sh
curl -fL --progress-bar --show-error \
  https://github.com/blazncloud/blazn/releases/download/v0.1.0-poc.132/install.sh |
  BLAZN_VERSION=v0.1.0-poc.132 sh
blazn version
```

The installer verifies the signed checksum manifest. The CLI talks to
`https://api.blazn.frontro.com` by default.

## 2. Sign up or sign in

```sh
blazn auth login
```

The CLI prints a one-time device code and opens the activation page. Enter your
email address, choose sign-up or sign-in, and type the six-digit code that
arrives by email (from `no-reply@mail.blazn.benpelo.com`; it expires after 10
minutes). There are no passwords.

```sh
blazn auth status
blazn auth logout      # revokes this device's session
```

## 3. Create and manage a workspace

Mutating commands take a `--request-id` so a retry is safe.

```sh
blazn workspace create "My team" --request-id ws-$(date +%s)
blazn workspace list
blazn workspace use WORKSPACE_ID
blazn project create "First project" --request-id proj-$(date +%s)

blazn workspace invite WORKSPACE_ID --role member --request-id inv-$(date +%s)
# the teammate, signed in on their own machine:
printf '%s\n' "$INVITE_TOKEN" | blazn workspace join --invite-stdin --request-id join-$(date +%s)

blazn workspace members WORKSPACE_ID
blazn workspace set-role USER_ID --role viewer --expected-version N --workspace WORKSPACE_ID --request-id role-$(date +%s)
blazn workspace remove-member USER_ID --expected-version N --workspace WORKSPACE_ID --request-id rm-$(date +%s)
blazn workspace leave --workspace WORKSPACE_ID --request-id leave-$(date +%s)
```

Roles are `owner`, `administrator`, `operator`, `member` and `viewer`. The last
owner cannot leave or be demoted.

## 4. Register a machine as a node

Requirements today:

- a fresh Ubuntu 26.04 amd64 machine or VM with `sudo` (the qualification VM
  has 4 CPUs and 8 GB of memory);
- on the same network as the Frontro cluster, because the node joins it as a
  MicroK8s worker (cluster API `192.168.0.108:16443`).

On that machine, signed in with a workspace selected:

```sh
blazn node install
blazn node list --workspace WORKSPACE_ID
```

`node install` downloads the pinned MicroK8s package, joins the cluster, and
activates the node. It takes a few minutes. The node carries the taint
`blazn.dev/sandbox-node=true:NoSchedule`, so only sandboxes run on it. If a
step fails, the install rolls itself back.

```sh
blazn node uninstall --yes
```

Uninstall drains the node, leaves the cluster, removes what the install added,
and retires the node in the API.

## 5. Create a sandbox

Sandboxes are created from a template published in your workspace, and run on
your registered nodes. Templates are per workspace, so publish one first. The
qualified template is `coding-agent@go-1.26.2-node-22.19.0-ghcr-1`, whose
images are pinned by digest on public GHCR (`ghcr.io/blazncloud/*`).

```sh
blazn template validate -f template.yaml
blazn template publish -f template.yaml --workspace WORKSPACE_ID --request-id tpl-$(date +%s)

blazn sandbox create --template NAME@VERSION --arch amd64 --mode direct --expires 30m \
  --source REPOSITORY=COMMIT_SHA \
  --approved-non-sensitive --workspace WORKSPACE_ID --request-id sbx-$(date +%s)
blazn sandbox get SANDBOX_ID                 # wait for "ready"
blazn sandbox exec SANDBOX_ID -- uname -a
blazn sandbox upload SANDBOX_ID ./file /workspace/src/REPOSITORY/file
blazn sandbox download SANDBOX_ID /workspace/src/REPOSITORY/file ./file.copy
blazn sandbox stop SANDBOX_ID --request-id stop-$(date +%s)
blazn sandbox delete SANDBOX_ID --request-id del-$(date +%s)
```

File transfers need a mounted workspace volume. A source checkout
(`/workspace/src/REPOSITORY/`) is one; `/workspace/tmp` is not mounted, and an
upload there fails with `sandbox_upload_failed`.

**Status:** this lifecycle passed twice on a registered node on 2026-10-01
with no leftover Pods, Sandbox objects or active grants (plan item M4.2).
Known gaps, tracked in the delivery plan:

- **A fresh node needs one manual step before it runs a sandbox (M4.7).** The
  shared scheduling queue only places sandboxes on nodes labeled
  `frontro.io/agent-workloads=true`, and node activation does not add that
  label yet. A cluster administrator adds it:
  `kubectl label node NODE_NAME frontro.io/agent-workloads=true`.
- A create that fails before the controller records it (for example an
  unschedulable Pod) leaves a sandbox that the normal stop and delete cannot
  clean up (M4.8).
- The example templates in this repository do not point at GHCR yet (M4.9).

## Automated checks

```sh
python3 infra/qualification/qualify-flows.py        # steps 2 and 3, 42 checks
python3 infra/qualification/sandbox-lifecycle.py … # step 5, 13 checks per iteration; see infra/qualification/README.md
```

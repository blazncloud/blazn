# Blazn end-to-end delivery plan

Updated 2026-09-30 (M3 complete). This plan replaces the ordered gates in
[`development-milestones.md`](development-milestones.md) as the source of truth for status.
That file keeps its execution log and evidence rules.

## Goal

A person signs up and creates a workspace, then invites and removes teammates. They register their
own machines as nodes and create sandboxes on those nodes. They run agents in the sandboxes using
**any** harness, then talk to those agents from the CLI, a webhook, Slack, email, SMS, or any other
channel. Everything runs on Blazn's own infrastructure, with nothing depending on a personal
account or domain.

A milestone is **done** only when its acceptance proof passes against the hosted environment
(`api.blazn.frontro.com`) using a published CLI release. Passing unit tests and synthetic sessions
count as evidence, but not as acceptance. This is the same rule as the delivery ledger.

## Status at a glance

| # | Milestone | Status | Depends on |
|---|---|---|---|
| M0 | Hosting baseline, repo hygiene, licensing | In progress | — |
| M1 | Identity: email-code sign-up and sign-in | **Done** | M0 |
| M2 | Workspaces, projects, membership | **Done** | M1 |
| M3 | Node registration and lifecycle | **Done** (M3.1 passed on Frontro with poc.132) | M2 |
| M4 | Sandboxes (virtual environments) on registered nodes | **In progress** | M3 |
| M5 | Model access: scoped provider credential and proxy | Built (proxy contract); not qualified live | M2 |
| M6 | Real agent execution and two-way messaging | Partly built; execution path is synthetic today | M4, M5 |
| M7 | Bring-your-own harness | Contract only | M6 |
| M8 | Channels foundation: endpoints, triggers, webhooks | Designed in product overview; no code | M6 (contracts can start now) |
| M9 | Slack channel | Not started | M8 |
| M10 | Email channel (aliases, inbound and outbound) | Not started | M8 |
| M11 | SMS and additional channels | Not started | M8 |
| M12 | Release, recovery, operations, open-source launch | Partly done (signed releases) | runs alongside everything |

Critical path: **M3 → M4 → M6 → M8 → M9/M10/M11**. M5, M7 and M12 run in parallel.

---

## M0 — Hosting baseline, repo hygiene, licensing

Current state:
- Control API `api-dev` runs in namespace `blazn-test` on ben5 and is served at `api.blazn.frontro.com`.
- Postgres 17 runs on frontro-db-1 (database `blazn_test`), with an in-cluster object store and registry.
- The sandbox controller runs in `blazn-poc-system`.
- The node broker runs on ben1 and the MicroK8s worker issuer runs on ben1.
- The throwaway qualification cluster runs on ben4.
- The repository is **public but has no license**, so it is not open source yet.

| Task | Acceptance |
|---|---|
| M0.1 Choose and add a license (recommended: Apache-2.0), `CONTRIBUTING.md`, `SECURITY.md` | License file on `main`; README badge |
| M0.2 ✅ Commit the hosting manifests (`infra/local-test/` is uncommitted) as a reviewed `infra/frontro/` overlay — done 2026-10-01: `infra/frontro` is exported from the live cluster and `kubectl diff` is empty | A clean apply from the repo reproduces `api-dev`, broker, issuer and controller |
| M0.3 ✅ Hosting runbook: components, hosts, secrets locations (paths only), deploy and rollback steps — done 2026-10-01: `docs/delivery/hosting-runbook.md` | `docs/delivery/hosting-runbook.md` reviewed |
| M0.4 ✅ Remove the temporary DB exception for the test cluster (done 2026-09-30); give the test cluster its own database before its next cycle | The rule and the line are gone (done); the test cluster uses its own database |
| M0.5 Rotate the Resend sending key that was shared in chat | New key in the secret store; old key revoked |
| M0.6 (partly done) Pin MicroK8s: `snap refresh --hold microk8s` on every cluster host, plus a reviewed procedure for adding a new revision to the issuer's allowlist (the issuer pins 9072 and 9075) — holds applied 2026-10-01 on ben1–ben5 and Mac minis 1, 2, 5 (3, 4, 6 unreachable); the revision procedure is in the hosting runbook | Hold applied on all hosts (ben1 and the test control plane done 2026-09-30); the procedure is documented |
| M0.9 Enable the `NodeRestriction` admission plugin on the Frontro API server (today any kubelet credential, including a Blazn node's, can patch every Node) — blocked on a product change: `NodeRestriction` forbids a kubelet from changing its own taints, which node activation and uninstall do today (see the hosting runbook) | `auth can-i patch nodes/<other> --as=system:node:<self>` is `no`; existing workers unaffected |
| M0.7 Clean up 27 stale `Init:Unknown` pods in `blazn-poc-sandboxes` and 11 stale `active` grant rows | Zero orphaned sandbox pods and grants; this needs explicit authorization |
| M0.8 Replace `blazn.benpelo.com` in contract `$id` URIs and client generators — **Done** 2026-09-30 (#238; the old POC infra that still named it was removed) | `grep` finds no personal domain in shipped artifacts |

## M1 — Identity ✅

Delivered:
- Passwordless email-code sign-up and sign-in (six-digit codes stored as an HMAC, 10-minute TTL, five attempts), sent through Resend from `no-reply@mail.blazn.benpelo.com`.
- CLI device flow, `auth status`, `auth logout` with server-side revocation.
- Private capture inbox for `@blazn.invalid`, so automated tests can read codes themselves.

Remaining, not blocking:
- M1.1 Move the sender to a Blazn-owned domain.
- M1.2 Browser session for the web UI, once the frontend source exists. This is the ledger's M0 blocker.
- M1.3 Optional TOTP/MFA and account recovery.

## M2 — Workspaces, projects, membership ✅

Delivered and qualified 34/34 by `infra/qualification/qualify-flows.py`:
- sign-up, login and logout;
- create, list, rename and select a workspace;
- create and list projects;
- invite and join, role enforcement, revoking an invite, and sign-in again.

| Task | Acceptance |
|---|---|
| M2.4 ✅ (done 2026-10-01: `qualify-flows.py` passes 42/42 with poc.132) Add member removal, `set-role`, `leave`, and last-owner protection to `qualify-flows.py` (the CLI already has `remove-member`, `set-role`, `leave`) | A removed member's next call is denied; the last owner cannot leave or be demoted |

## M3 — Node registration and lifecycle ✅

Delivered (#211–#235, releases poc.122–poc.132):
- Signed install plans, an enrollment exchange, and a transactional install with rollback.
- The MicroK8s worker issuer and node broker.
- A bootstrap taint and quarantine until activation.
- Activation, heartbeats, and server-side retirement (the node becomes `removed`/`revoked`, its identity is revoked, and an audit event is written).
- Isolation from the shared cluster (#233): a permanent `blazn.dev/sandbox-node=true:NoSchedule` taint that only sandbox Pods tolerate, and the load-balancer exclusion label, so Frontro's ingress, MetalLB speaker and log shippers never run on a user's machine.
- Clean removal (#233): uninstall evicts with a `blazn.dev/retired` NoExecute taint, runs `microk8s leave`, and retirement deletes the Node object through the broker and issuer.

| Task | Acceptance |
|---|---|
| M3.1 ✅ One real node install into the Frontro cluster (poc.132, 2026-09-30) | Passed: `active`/eligible, only tolerate-all DaemonSets on the node, the traefik VIP never moved, uninstall `removed` with 0 residues, Node deleted, 0 orphans |
| M3.2 ✅ Uninstall leaves the cluster and the Node object is removed | Passed on the test cluster (×3) and Frontro |
| M3.3 `node-e2e` added to the qualification suite with a single command | One command installs, uninstalls and verifies against the test cluster |
| M3.4 macOS and Mac mini nodes (Lima binding) | A Mac node registers and runs a sandbox (can follow M6) |

## M4 — Sandboxes on registered nodes

Built:
- Sandbox templates and versions, sandbox create/list/get/events/operations, and access grants with `exec` and file upload/download.
- Artifacts, the sandbox controller (running), and the Agent Sandbox and Kueue integration.
- CLI: `blazn sandbox create|get|list|exec|upload|download|stop|watch`, `blazn template …`.

Not yet proven on the hosted stack.

Prerequisites for sandboxes on Blazn nodes (from M3's isolation taint):
- #236: the controller accepts the 26 existing Sandboxes created before the sandbox-node toleration.
- Roll out the sandbox controller built from `main`, then the phase-5 boundary policy that allows exactly the sandbox-node toleration. The live boundary transaction journal is missing and must be re-sealed from the live objects before `upgrade-boundary.sh` can run.

| Task | Acceptance |
|---|---|
| M4.1 Publish a first approved template (Ubuntu with a toolchain) to `api-dev` | `template list` shows it; its digest is pinned |
| M4.2 Qualify the lifecycle on the M3.1 node: create → ready → exec → upload/download → stop → delete | Automated test step passes twice; zero residual pods, PVCs or grants |
| M4.3 Placement: a sandbox lands only on eligible, active nodes in the owner's workspace | A node from another workspace is never selected; a quarantined node is never selected |
| M4.4 Policy denials: network egress, resource caps, expired or revoked grants | Each denial is observed and audited |
| M4.5 `sandbox watch` replays stop, expiry and delete events (known gap) | Watch shows the terminal event |
| M4.6 Hardened runtime (gVisor or Kata RuntimeClass) before any multi-tenant or public use | The RuntimeClass is qualified; untrusted-code isolation is documented |

## M5 — Model access

Built: the proxy contract and activation core (OpenAI and Anthropic request, response, stream and error normalization).

| Task | Acceptance |
|---|---|
| M5.1 A workspace-scoped provider credential, stored in the secret store and never exposed to a sandbox | A sandbox calls the model only through the proxy; direct egress is denied |
| M5.2 Budget and rate limits per workspace | Over-budget requests are refused and audited |
| M5.3 Local model route (Qwen or DeepSeek on the Sparks) plus one approved fallback | Fallback happens at most once and is recorded |

## M6 — Real agent execution and two-way messaging

Built:
- Agents and versions; harness definitions, versions and profiles.
- Runs with create/list/get/cancel, messages, events, progress and artifacts.
- CLI `run send|messages|watch|logs|result|cancel`.
- `blazn-harness-worker`, with only the Hermes adapter wired.

**Gap:** nothing schedules a run into a sandbox. Runs only advance through the synthetic `claim`, `deliver`, `synthetic-progress` and `synthetic-complete` endpoints.

| Task | Acceptance |
|---|---|
| M6.1 Run dispatcher: a fenced, lease-based controller that takes a queued run, creates or claims a sandbox from the agent's template, and launches the harness worker in it | A run moves `queued → running → succeeded` without any synthetic endpoint |
| M6.2 Message relay: user messages reach the running harness, and harness output streams back as run events | `run send` gets a reply within the same run; `run watch` streams it |
| M6.3 `codex-cli` and `claude-code` adapters in the harness worker, next to Hermes | The same portable coding agent passes through all three |
| M6.4 Cancellation, timeout and recovery (API restart, node loss, worker crash) | Cancellation cleans up; a restart resumes or fails the run cleanly with no orphans |
| M6.5 Artifacts: a coding run returns a patch without pushing | The patch artifact downloads and applies |
| M6.6 Provenance: model, node, template, agent version, cost and time recorded on every run | Visible in `run get` |
| M6.7 Add agent runs to the qualification suite | Test run: agent run, follow-up message, cancel |

Gate: a real, authorized model workload completes end to end twice from a clean namespace.

## M7 — Bring-your-own harness

Contract: the `generic-cli` adapter kind, harness conformance schema, `poc-harness-restricted-v1` security policy, and the approved/deprecated/prohibited status.

| Task | Acceptance |
|---|---|
| M7.1 Harness registration: `blazn harness publish-definition` for `generic-cli` with a declared executable digest, input mode, output parser and capabilities | A new harness is registered without changing any code |
| M7.2 Approval workflow: workspace admins approve or prohibit a harness version, and runs refuse unapproved ones before creating a sandbox | An unapproved harness fails with an actionable compatibility error |
| M7.3 Conformance kit: `blazn harness doctor` plus a test fixture any harness author can run | A fourth harness (for example OpenCode or Aider) passes conformance |
| M7.4 Harness images: build a harness into a template via the development workflow | A custom harness image runs a run end to end |
| M7.5 Author documentation | `docs/harness-authoring.md` |

## M8 — Channels foundation (endpoints, triggers, deliveries)

Design source: product overview, "Triggers, endpoints, and email aliases". It defines the Endpoint,
Endpoint binding, Trigger envelope, Trigger definition, Delivery, and Conversation binding.

| Task | Acceptance |
|---|---|
| M8.1 Freeze contracts: `channels.openapi.json` plus schemas for Endpoint, Binding, Trigger, Envelope, Delivery, Conversation binding | Contract tests and generated clients |
| M8.2 Persistence and management API/CLI: `blazn endpoint create|list|get|disable`, `blazn trigger create|test|activate` | CRUD through the API with an audit trail |
| M8.3 Inbound front door: one public ingress that verifies signatures, deduplicates, rate-limits, and normalizes to an envelope | Replayed or forged requests are rejected; duplicates do not create a second run |
| M8.4 Identity mapping: an external sender (Slack user, email address, phone number, webhook key) maps to a workspace member or a restricted guest principal | Unmapped senders get the endpoint's configured policy (deny or guest) |
| M8.5 Conversation binding: an external thread maps to a Blazn session or run, so replies continue the same run | A follow-up in the same thread reaches the same run |
| M8.6 Outbound reply router: run events and results go back to the originating channel, with retries and a dead-letter queue | A reply is delivered or visibly dead-lettered |
| M8.7 **Webhook channel** (first channel): signed inbound webhook starts or continues a run; optional signed outbound callback | An end-to-end webhook starts a run and the callback receives the result |
| M8.8 Abuse controls: per-endpoint quotas, allowlists, content size limits, and loop prevention | A trigger storm is throttled and audited |

## M9 — Slack

| Task | Acceptance |
|---|---|
| M9.1 Slack app (OAuth install per workspace, bot token in the secret store), bound as an Endpoint | Install and uninstall from a Blazn workspace |
| M9.2 Mention or DM an agent; the thread becomes the conversation binding; progress and results post in-thread | A mention starts a run and a thread reply continues it |
| M9.3 Slash command or shortcut to pick an agent; interactive approvals (buttons) | An approval click resumes a paused run |
| M9.4 Slack user maps to a Blazn member | A non-member is denied or treated as a guest per policy |

## M10 — Email

| Task | Acceptance |
|---|---|
| M10.1 Email aliases per agent, project or team (for example `agent-name@<workspace>.blazn…`) on a Blazn-owned domain | Alias created through the API/CLI |
| M10.2 Inbound through Resend inbound or an MX route to the front door; parse and quarantine attachments | Sending mail to the alias starts a run |
| M10.3 Threading using `Message-ID`, `In-Reply-To` and `References`, bound to a conversation | A reply continues the same run |
| M10.4 Outbound replies from the alias; handling for bounces, complaints and suppression; loop prevention | Replies are delivered; auto-replies do not loop |

## M11 — SMS and additional channels

| Task | Acceptance |
|---|---|
| M11.1 SMS through Twilio (number per endpoint, signed status callbacks, STOP/HELP compliance) | A text starts a run, and a reply SMS returns the result |
| M11.2 Pluggable channel adapter interface, so new channels (Discord, Teams, WhatsApp, voice) are adapters behind M8 | A new adapter needs no core schema change |
| M11.3 Web widget / Blazn Button (depends on the frontend) | The embedded widget starts a run |

## M12 — Release, recovery, operations, open-source launch

Done: signed CLI releases (candidate → materials rotation → publish), curl installer, signed node plan materials.

| Task | Acceptance |
|---|---|
| M12.1 CI deploy automation for `api-dev` (image build, migrate, rollout, smoke test) | Merging to `main` deploys to development with no manual steps |
| M12.2 Backups: Postgres PITR on frontro-db-1 and object-store replication, plus a restore rehearsal | A restore into a scratch database matches |
| M12.3 Rollback rehearsal for the API, controller, broker and node plan materials | Documented and timed |
| M12.4 Monitoring and alerts: API health, broker and issuer health, DB pool errors, snap revision drift, stuck runs | Alerts fire in a drill |
| M12.5 Production environment separate from development (own namespace, database and domain) | Production cut over with explicit authorization |
| M12.6 Open-source launch: license (M0.1), public docs, contribution guide, security policy, Homebrew tap | Public announcement checklist complete |

---

## Known risks

- **Snap auto-refresh** silently moved a cluster host to an unpinned MicroK8s revision on 2026-09-30, and node registration stopped until it was reverted (M0.6).
- **Shared DB restarts** broke the broker's pool until it was restarted. The broker and API need pool recovery and a health signal (M12.4).
- **Isolation**: sandboxes are orchestration-isolated only until M4.6. Do not run untrusted third-party code or expose public endpoints before then.
- **Frontend source** is still inaccessible, which blocks the browser session, the web channel and the Blazn Button.

## How to verify

- `infra/qualification/qualify-flows.py`: identity, workspaces, projects, membership (extend per M2.4, M4.2, M6.7, M8.7).
- The node test cycle on the ben4 throwaway cluster: install, register, uninstall, retire (M3.3 turns this into one command).

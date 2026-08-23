# Agent and Harness Adapter contract v1alpha1

This is the Phase 7 contract freeze. It adds no routes, database tables,
controller, adapter executable, credentials, or live workload. The contract
uses the existing Workspace and Project tenancy, Run and Operation lifecycle,
Artifact identity, Sandbox allocation, and proxy route decision rather than
creating harness-specific copies.

The normative schemas are `agent.schema.json`, `harness.schema.json`,
`harness-run.schema.json`, and `harness-conformance.schema.json`. An `Agent` is
stable; every `AgentVersion` is immutable and binds exact instructions, source
commit, model policy version, SandboxTemplateVersion, tools, resource profile,
allowed HarnessProfiles by exact semantic digest, required capabilities,
evaluation, and output shape. A Profile edit creates a different selection and
cannot alter an already published AgentVersion by retaining the same ID.

`HarnessDefinition` names one approved adapter kind. `HarnessVersion` freezes
its digest-pinned package or image, typed executable/argv, parser and worker
protocol, platform support, capabilities, credential capabilities,
compatibility, and reviewed provenance. `HarnessProfile` is workspace-scoped
and selects one exact HarnessVersion, model route version, capability-scoped
credential leases, tools, bounded scalar overrides, and restrictive policy.
It never stores a bearer token, provider key, kubeconfig, node credential,
private key, arbitrary environment map, or interpolated shell command.
AgentVersion, the full HarnessVersion implementation and provenance, and every
HarnessProfile have separately domain-separated canonical SHA-256 identities.
Changing an executable argument, parser, capability, package/image, review,
credential declaration, model route, tool, or override changes its identity.
Executable definitions use a fail-closed identity and exact-path allowlist for
the reviewed adapter kind. Renamed or versioned shells, interpreters, network
clients, and aliases are refused along with command chaining, expansion, pipes,
and redirection in fixed arguments.

## Pre-Sandbox compatibility

The control plane compares every required AgentVersion capability with the
selected published HarnessVersion before queueing or creating a Sandbox. A
missing conversation, resume, event, tool, approval, model, output, recovery,
or cancellation capability returns the exact missing set with `sandboxId=null`.
The terminal refusal contains only the control-plane capability error and
terminal receipt: it has no Sandbox, Node, credential lease, proxy or fallback
decision, Message, adapter/harness/sandbox execution event, model or tool event,
follow-up or resume evidence, output Artifact, or model usage. The capability
error and terminal receipt have exact payloads with no adapter extension data,
and billing is an explicit zero-cost control-plane receipt. The capability error
binds the canonical sorted missing set recorded by compatibility. No
Cancellation or cleanup flag can claim requested, acknowledged, terminated,
revoked, handled, or completed work for this execution-free refusal. No
requirement is silently dropped. A fallback is either absent or one
approved attempt whose exact route, authorization, and approval receipt are
frozen in the AgentVersion. Runtime evidence orders the authoritative primary
request, primary proxy decision, primary failure, control-plane authorization,
one fallback request, and fallback proxy decision and binds their route, policy,
request, event, authorization, and receipt identities. Fallback remains a
pre-output transition: primary usage, an assistant or tool response, a tool or
approval event, an Artifact or patch effect, or a later logical model request
before fallback authorization makes the fallback ineligible. Assistant and
tool responses are classified from the resolved Message role, not from an
adapter-controlled event label, so relabeling output as user input cannot retain
fallback eligibility. The entire
normalized stream contains exactly one request to the frozen fallback route,
that request is the recorded `requestEventId` with `attempt=1`, and a second
request or differently numbered attempt is refused.
Credential declarations use typed route, repository, or tool scopes. Every
run-scoped lease must exactly match one declaration, the Agent's model route,
repository, or tool set, and its declared lifetime; terminal Runs retain only
revoked lease metadata and never secret material. Nonterminal Runs may record
an active lease with `revokedAt=null`; terminal Runs require an ordered
revocation timestamp for every lease. A capability refusal records no lease at
all. Issuance occurs within the Run lifecycle before its first event, and every
lease remains unexpired through the authoritative observed lifecycle.
Declaration capability/scope pairs and lease IDs are unique, with one lease for
every declaration and exactly one model lease bound to the selected Agent route.
Authenticated proxy profiles must use `model.proxy`. A broader provider
credential is refused unless the Profile resolves an approved, same-Workspace
DIRECT authorization bound to the exact Profile, route version, capability,
and provider. The Run records that authorization ID in a matching `DIRECT`
decision authenticated with provider-capability proof; proxy-routed Runs cannot
silently become direct.

## Normalized execution

All adapters use the same Run, Operation, Sandbox, Session, Message, Event,
Result, and Artifact resources. Events have monotonic sequence and resumable
cursors and cover preparation, start/wait/resume/stop/exit; user, assistant,
and tool messages; causally ordered tool and approval decisions; model/usage
references; monotonic observation times and authoritative event sources;
progress, patch, Artifact, cancellation, timeout, and terminal result. Adapter
detail may appear only below a namespaced `hermes.*`, `codex.*`, `claude.*`, or
`generic.*` extension. It cannot override core identity or authoritative Run
state. The source-authority table is exhaustive over the closed event type
enumeration, so no event type receives implicit source authority. Extension
payloads are bounded scalar maps and recursive credential scanning rejects
canonical provider keys, separated secret-key names, and credential-like
values. Secret-key suffix matching is case-normalized, known provider token
forms are rejected even under neutral metadata keys, and the allowlist contains
only explicit non-secret authorization metadata. Every normalized tool event
must name a tool allowed by both the immutable AgentVersion and selected
HarnessProfile.

Compatible Runs bind the selected proxy decision to one exact normalized
`model.proxy-decided` event and its earlier `model.requested` event, including
the exact frozen route identity and version. Every other request uses that same
primary route unless it is the single frozen fallback request, and each closed
`model.usage` payload repeats the route identity and version of its exact earlier
request. Usage for the proxy-bound primary request or approved fallback request
must follow that request's exact authoritative route-decision event. These
authority and ordering checks apply to every compatible snapshot while a Run is
still active as well as after it terminates. A billing event, whenever present,
must exactly bind the authoritative pricing receipt and follow all normalized
model request, route-decision, fallback, and usage evidence it summarizes;
terminal Runs require it. Terminal model-request counts and input/output token
totals equal the normalized request and usage events rather than uncorroborated
adapter counters.
One closed `billing.recorded` event binds the authoritative pricing and receipt
identity; control-plane and proxy receipts have matching authoritative event
sources.

Follow-ups retain the Session and Conversation IDs. Resume increments a
generation and resumes event/message cursors. Cancellation is complete only
after acknowledgement, process-tree termination, credential revocation,
Artifact handling, and cleanup. A harness exit cannot turn a cancelled Run
into success.

Nonterminal Runs have no terminal Result, completion timestamp, or terminal
event. Terminal Runs require all three to agree with the authoritative Run
status and receipt, and a compatible terminal Run retains its allocated
Sandbox identity. A successful Run has no cancellation or process-termination
claim and requires completed cleanup, credential revocation, and Artifact
handling. Once cancellation is acknowledged, a Run cannot succeed.
Acknowledged cancellation may terminate only as fully evidenced `cancelled` or
as `recovery_required`; it cannot be relabeled failed or timed out. Every event
observation is at or after Run creation and, for terminal Runs, at or before
Run completion. The single terminal event is observed exactly at the
authoritative completion timestamp.

Messages are normalized resources with exact Run, Session, Conversation,
generation, ordinal, parent, follow-up target, role, content-digest, and creation
time identity. Every Message has exactly one normalized message event, and every
normalized message event resolves one Message exactly once;
its event type, authoritative source, and observation time equal that Message's
role and creation time. An assistant or tool Message cannot be relabeled as a
control-plane user event, replayed through a second message event, or hidden by
omitting its event. Fallback eligibility also inspects resolved assistant and
tool Message creation times, so an omitted event cannot erase pre-output proof.
Parents and follow-up targets must precede the child, empty cursors remain zero,
and resume generations are contiguous and occur only after the matching
follow-up cursor boundary. Each generation has exactly one follow-up Message
and one unique acceptance event. Cancellation likewise uses one ID shared by
one request and its later acknowledgement.
Terminal patch, summary, and other result Artifacts resolve to the same tenant
with distinct role, kind, media type, and content digest. The complete terminal
snapshot is bound by a domain-separated receipt digest.
Agent output declarations are closed to the runnable `patch`, `summary`, and
`output` roles so publication cannot create an AgentVersion that no adapter can
complete. A successful Run always names the exact resolved patch Artifact in
`patchArtifactId`.

At run time the selected Profile digest, model route, tools, DIRECT
authorization, credential declarations, and leases are revalidated against the
immutable AgentVersion rather than trusting publication-time validation.
The same selected-profile compatibility checks are rerun for each standalone
portable evaluation before its passing receipt is accepted. A Run also binds
the canonical Agent ID, not only its AgentVersion ID.

Run provenance captures exact AgentVersion, HarnessVersion, HarnessProfile,
SandboxTemplateVersion, source repository/commit, model route/version/protocol,
authenticated proxy decision, Node binding, worker protocol, terminal receipt,
usage, result, and Artifact IDs. Proxy outcomes remain `ROUTED`, `DIRECT`, or
`BYPASS`; the harness cannot infer routing from environment configuration.
The proxy proof includes request and event IDs, exact route and policy IDs and
versions, and the separate proxy workload authentication class.
Cost provenance requires a pricing identity/version and an authoritative
receipt. Priced proxy-routed execution uses a proxy receipt. DIRECT execution
and explicit `zero-cost` USD evidence use a control-plane receipt; zero-cost
evidence has zero micros and priced execution has a positive amount. Terminal
duration must equal the authoritative Run lifecycle timestamps.

## Fixtures and portable evaluation

Hermes, Codex CLI, Claude Code, and generic CLI fixtures validate against the
same schemas. The generic fixture proves extensibility without new Agent, Run,
Conversation, Message, Sandbox, or event types. The portable coding evaluation
uses one AgentVersion and exact source commit and requires no push, a patch and
summary, exact source, normalized events, no secret output, cleanup, and the
same core resources. Conformance also exercises capability refusal before a
Sandbox, follow-up, resume, tool/approval normalization, graceful process-tree
cancellation, terminal Artifacts, and credential cleanup.
Each result resolves the exact HarnessDefinition, HarnessVersion, and
HarnessProfile and binds one distinct Run receipt to the same AgentVersion,
source, SandboxTemplateVersion, task, and role-specific conformance Artifacts.
The resolved bundle is revalidated semantically, the Profile must be in the
AgentVersion allowlist, the portable evaluation ID must be the AgentVersion's
committed evaluation, and the Run receipt plus every evidence Artifact remain
in the Agent workspace.
Parity requires all four adapter kinds, a common parity group and evaluation
identity, identical evidence roles, and distinct Run, receipt, Artifact, and
content identities.

Run the pinned contract proof with `make test-harness-contract`.

## Deferred runtime gates

Gate 7 remains incomplete until Agent/Harness persistence and API authorization,
controller leases/finalization, vault leases, model/proxy integration, the four
adapters, disposable conformance, AMD64/ARM64 Sandbox execution, cancellation
and restart recovery, Artifact export, and the portable parity evaluation pass
end to end. Integrated Runs also depend on qualified Gate 5 templates, Gate 6
publication, Gate 6A routes, eligible capacity, and serialized production
publication approval.

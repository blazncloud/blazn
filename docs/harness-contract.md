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
allowed HarnessProfiles, required capabilities, evaluation, and output shape.

`HarnessDefinition` names one approved adapter kind. `HarnessVersion` freezes
its digest-pinned package or image, typed executable/argv, parser and worker
protocol, platform support, capabilities, credential capabilities,
compatibility, and reviewed provenance. `HarnessProfile` is workspace-scoped
and selects one exact HarnessVersion, model route version, capability-scoped
credential leases, tools, bounded scalar overrides, and restrictive policy.
It never stores a bearer token, provider key, kubeconfig, node credential,
private key, arbitrary environment map, or interpolated shell command.

## Pre-Sandbox compatibility

The control plane compares every required AgentVersion capability with the
selected published HarnessVersion before queueing or creating a Sandbox. A
missing conversation, resume, event, tool, approval, model, output, recovery,
or cancellation capability returns the exact missing set with `sandboxId=null`.
No requirement is silently dropped and no fallback attempt is created unless
the immutable AgentVersion permits one approved attempt.

## Normalized execution

All adapters use the same Run, Operation, Sandbox, Session, Message, Event,
Result, and Artifact resources. Events have monotonic sequence and resumable
cursors and cover preparation, start/wait/resume/stop/exit; user, assistant,
and tool messages; tool and approval decisions; model/usage references;
progress, patch, Artifact, cancellation, timeout, and terminal result. Adapter
detail may appear only below a namespaced `hermes.*`, `codex.*`, `claude.*`, or
`generic.*` extension. It cannot override core identity or authoritative Run
state.

Follow-ups retain the Session and Conversation IDs. Resume increments a
generation and resumes event/message cursors. Cancellation is complete only
after acknowledgement, process-tree termination, credential revocation,
Artifact handling, and cleanup. A harness exit cannot turn a cancelled Run
into success.

Run provenance captures exact AgentVersion, HarnessVersion, HarnessProfile,
SandboxTemplateVersion, source repository/commit, model route/version/protocol,
authenticated proxy decision, Node binding, worker protocol, terminal receipt,
usage, result, and Artifact IDs. Proxy outcomes remain `ROUTED`, `DIRECT`, or
`BYPASS`; the harness cannot infer routing from environment configuration.

## Fixtures and portable evaluation

Hermes, Codex CLI, Claude Code, and generic CLI fixtures validate against the
same schemas. The generic fixture proves extensibility without new Agent, Run,
Conversation, Message, Sandbox, or event types. The portable coding evaluation
uses one AgentVersion and exact source commit and requires no push, a patch and
summary, exact source, normalized events, no secret output, cleanup, and the
same core resources. Conformance also exercises capability refusal before a
Sandbox, follow-up, resume, tool/approval normalization, graceful process-tree
cancellation, terminal Artifacts, and credential cleanup.

Run the pinned contract proof with `make test-harness-contract`.

## Deferred runtime gates

Gate 7 remains incomplete until Agent/Harness persistence and API authorization,
controller leases/finalization, vault leases, model/proxy integration, the four
adapters, disposable conformance, AMD64/ARM64 Sandbox execution, cancellation
and restart recovery, Artifact export, and the portable parity evaluation pass
end to end. Integrated Runs also depend on qualified Gate 5 templates, Gate 6
publication, Gate 6A routes, eligible capacity, and serialized production
publication approval.

# Blazn Product Overview

**Status:** Vision draft  
**Audience:** Founders, product, design, engineering, and early collaborators  
**Scope:** High-level product definition; detailed requirements and architecture will follow

## Index

- [The idea](#the-idea)
- [The problem](#the-problem)
- [Product promise](#product-promise)
- [Who it is for](#who-it-is-for)
- [The product](#the-product)
- [System design](#system-design)
  - [System component index](#system-component-index)
  - [Nodes](#nodes)
    - [Local model capacity](#local-model-capacity)
  - [Blazn Agent Harness](#blazn-agent-harness-system)
  - [Smart LLM Router](#smart-llm-router)
  - [LLM Router Policy](#llm-router-policy)
- [How the pieces fit](#how-the-pieces-fit)
- [Product principles](#product-principles)
- [Brand direction](#brand-direction)
- [Initial product boundary](#initial-product-boundary)
- [What Blazn is not](#what-blazn-is-not)
- [Measures of success](#measures-of-success)
- [Documents to develop next](#documents-to-develop-next)

## The idea

Blazn is the operating workspace for an AI-enabled company.

It allows individuals and teams to interact with agents as a coordinated team at scale. People, agents, models, tools, knowledge, projects, and compute live in a shared workspace, so useful context and learning accumulate into a unified company brain instead of disappearing inside isolated chats and applications.

Blazn is available as a desktop application for macOS, Windows, and Linux, a CLI, and an optional managed cloud. A user can begin on one computer, invite a team, contribute additional machines as workers, and adopt managed infrastructure only where it is useful.

## The problem

AI work today is fragmented:

- Conversations and decisions are scattered across Slack channels, source code, email, documents, project tools, assistants, IDEs, and terminals.
- Agents operate independently, with limited knowledge of company goals, prior work, or one another.
- Local and cloud models require separate configuration, credentials, routing, and harness-specific integrations.
- Individuals and teams quickly exhaust AI subscription allowances when operating multiple agents, while direct API usage can become unpredictable and significantly more expensive at scale.
- Agents running directly on a local machine can consume its CPU, memory, storage, and thermal capacity, disrupting normal work or causing instability and restarts.
- A single local machine limits agent concurrency and throughput; scaling beyond it usually requires manually assembling more hardware or paying a cloud provider.
- Agent environments are difficult to provision consistently, secure, observe, and clean up.
- Runs produce logs and artifacts, but rarely create reusable organizational memory.
- Teams cannot easily understand what agents are doing, why they made a decision, what they cost, or whether their performance is improving.
- Project plans and agent execution live in separate systems, leaving people to translate roadmaps into prompts and results back into tasks.
- Connecting agents to a live product, customer issue, or support interaction usually requires custom infrastructure.

The result is a collection of powerful tools without a shared operating model. Teams spend time managing agents instead of working with them.

## Product promise

Blazn gives every person and team one place to:

1. Organize people and agents around shared objectives.
2. Run agents safely on local, contributed, or managed compute.
3. Use the right model for each request without changing tools.
4. Preserve work, knowledge, artifacts, and decisions as organizational memory.
5. Observe outcomes and help agents improve over time.
6. Connect agent work directly to projects, products, and customers.
7. Pool authorized capacity across company and employee machines, automatically provisioning isolated environments while protecting the owner's work.
8. Run agents through the Blazn Agent Harness while using local models, provider models, and Blazn cloud through one governed routing system.
9. Bring agents into existing applications through the Blazn Button so they can act on what a user is experiencing in the moment.

The experience should feel local-first, collaborative, inspectable, and progressively adoptable: valuable to one person on one machine, then capable of growing into a company-wide agent platform.

## Who it is for

### Individuals

Developers, founders, researchers, operators, and creators who want one interface for their agents, models, machines, projects, and reusable knowledge.

### Teams

Groups that want shared agents, governed access, coordinated execution, common artifacts, project visibility, and a durable record of how work was completed.

### Organizations

Companies that want a unified agent layer across local infrastructure and cloud services, with control over data, models, cost, security, and operational quality.

## The product

### 1. Blazn desktop application

The desktop application is the primary visual workspace on macOS, Windows, and Linux. It allows users to:

- Create, join, and manage workspaces.
- Invite people and manage roles, access, and shared resources.
- Talk with one agent or assemble a team of specialized agents.
- Follow live work, steer active runs, review decisions, and approve sensitive actions.
- Manage local machines, remote workers, sandboxes, and virtual environments.
- Browse agents, projects, runs, schedules, triggers, tools, resources, instructions, objectives, and metrics.
- Pin and share artifacts such as documents, plans, dashboards, reports, code changes, and research.
- Use the Blazn Agent Harness for conversations and work performed in an isolated environment or through an approved local-machine backend.

### 2. Blazn CLI

The CLI gives people and automation access to the same workspace and control plane. It is designed for terminals, scripts, CI systems, and agent-to-agent operation.

Core responsibilities include:

- Authentication and workspace selection.
- Agent creation, discovery, configuration, and invocation.
- Starting, inspecting, steering, and stopping runs.
- Managing nodes, environments, schedules, triggers, tools, and resources.
- Routing model requests through the Blazn AI Proxy.
- Publishing and retrieving artifacts.
- Exposing Blazn capabilities through automation-friendly and MCP-compatible interfaces.

The desktop app and CLI are two clients of the same product, not separate systems.

### 3. Workspaces and the company brain

A workspace is the shared boundary for a person or organization. It contains:

- Members, teams, roles, and policies.
- Agents and their identities, capabilities, objectives, and instructions.
- Knowledge, resources, tools, integrations, and credentials.
- Projects, roadmaps, milestones, tasks, and decisions.
- Runs, events, artifacts, metrics, schedules, and triggers.
- Nodes, execution environments, models, budgets, and usage.

The company brain is the connected, permission-aware memory formed by these elements. It is not simply a document store or transcript archive. It preserves relationships between objectives, decisions, work, evidence, outcomes, and the agents and people involved.

### 4. Agent library and teams

Users can create and manage a reusable library of agents. Each agent can have:

- A name, role, purpose, and measurable objectives.
- Instructions, skills, tools, resources, and LLM Router Policy preferences.
- Environment and machine requirements.
- Permissions, budgets, and escalation rules.
- Schedules, triggers, and event subscriptions.
- Run history, metrics, evaluations, and improvement history.

Agents may work independently, be assigned to projects, or collaborate as a team. Larger coordinating agents can delegate work, combine results, detect gaps, and help specialized agents improve how they work together.

### 5. Blazn Agent Harness

The Blazn Agent Harness is the product's canonical runtime for agents. It owns how an agent receives context, requests a model, uses tools, performs work, collaborates, emits events, pauses, resumes, and completes a run. Blazn does not depend on bring-your-own agent harnesses for its core execution model.

The harness supports:

- Persistent conversations and objective-driven sessions.
- Live progress, events, intermediate results, and structured approvals.
- Follow-up instructions and steering during a run.
- Context assembly from workspace memory, projects, resources, and artifacts.
- Tool and MCP access governed by agent and workspace permissions.
- Work in isolated sandboxes or approved local-machine backends.
- Checkpoints, pause, resume, cancellation, retry, and recovery.
- Temporary agents, delegation, handoffs, and coordinated agent teams.
- Durable outputs, evaluations, and introspection linked to run history.
- Model requests sent exclusively through the Smart LLM Router.

The desktop app, CLI, API, MCP server, schedules, triggers, and Blazn Button all initiate work through this same harness. External applications and developer tools may call Blazn APIs or use its AI Proxy, but they do not redefine Blazn's agent lifecycle or execution semantics.

The harness should make an agent's context, environment, tools, actions, model routing, approvals, and state visible without forcing users to understand the underlying orchestration system.

### 6. Nodes, workers, and environments

A user can choose to make a machine available as a Blazn node. Eligible work is then scheduled onto contributed macOS, Windows, or Linux capacity according to its capabilities and the workspace's policies.

Blazn can provision an isolated sandbox or virtual environment for a run, attach the required tools and resources, stream progress, preserve selected outputs, and clean up when the work is complete. It should support:

- Personal machines and team-owned hardware.
- Linux container and sandbox workloads.
- Native platform workloads, including work that requires macOS or Windows capabilities.
- Managed environments supplied by Blazn cloud.
- Capability-aware placement, queues, quotas, priorities, and concurrency controls.
- Clear consent, pause/drain controls, resource limits, and visibility for machine owners.

Kubernetes Agent Sandbox is a candidate foundation for selected persistent or interactive Linux environments. It is an implementation component, not a requirement for every workload or platform.

### 7. Blazn AI Proxy

The AI Proxy provides one compatible endpoint for the Blazn Agent Harness and external clients such as Codex, Claude Code, IDEs, and internal applications. Its Smart LLM Router evaluates every request against an effective LLM Router Policy before choosing where the request runs.

It can route requests to:

- Models running locally on a user's machine or team hardware.
- Third-party model providers configured by the workspace.
- Models and capacity offered by Blazn cloud.

Routing accounts for model capability, privacy, availability, health, queue depth, latency, cost, policy, and fallback behavior. A caller can request a logical model alias or a capability tier instead of binding an agent to one provider or machine. Users should be able to understand where a request went, why it went there, and whether a retry or fallback occurred, while existing external tools continue to work with minimal configuration.

The AI Proxy is a model-access surface, not an alternative agent harness. External tools may use its compatible APIs for model requests, while agents created and operated by Blazn always run through the Blazn Agent Harness.

This capability builds on the approach established by Blaze Proxy and brings it into the shared Blazn workspace.

### 8. Blazn cloud

Blazn cloud is optional managed infrastructure for teams that do not want to operate every component themselves. It can provide:

- Hosted workspace collaboration and synchronization.
- Managed model access and AI routing.
- On-demand agent environments and elastic execution capacity.
- Durable run history, artifacts, schedules, triggers, and organizational memory.
- Secure remote access to a user's authorized Blazn workspace.
- Administrative controls, usage reporting, policy, and billing.

Local and self-hosted use remain first-class. Cloud adoption should add capacity and convenience without making contributed machines or local models second-class citizens.

### 9. Runs, events, analytics, and improvement

Every meaningful execution is represented as a run. A run connects the initiating person or event, agent, objective, instructions, environment, model activity, tool calls, approvals, outputs, cost, timing, and outcome.

Structured events and metrics make runs observable in real time and reviewable afterward. They support:

- Progress and health monitoring.
- Cost, latency, reliability, and quality analysis.
- Auditing and incident investigation.
- Comparison across agents, models, tools, and workflows.
- Evaluation against the run's objective and expected outcome.

After a run, an agent can perform bounded introspection: identify what worked, what failed, and what reusable learning should be proposed. Improvements to instructions, tools, memory, or collaboration patterns should be evidence-based, versioned, and subject to workspace policy rather than silently rewriting an agent.

Coordinating agents can analyze patterns across multiple agents and runs to improve delegation, handoffs, shared tools, and team performance.

### 10. Projects and execution

Blazn includes project management so plans and agent work share the same context. Workspaces can organize:

- Product and company roadmaps.
- Milestones, projects, tasks, owners, dependencies, and status.
- Objectives, success measures, decisions, risks, and evidence.
- Human and agent assignments.

An agent run can begin from a task, update it with live progress, attach evidence and artifacts, surface blockers, and propose next work. People retain control of priorities and commitments while agents reduce the coordination overhead between planning and execution.

### 11. Artifacts and shared library

Blazn provides a durable, searchable home for useful outputs. Users can pin, organize, version, and share:

- Documents and research.
- Plans, specifications, and decisions.
- Dashboards and reports.
- Code changes and release evidence.
- Data, images, recordings, and other generated files.
- Reusable prompts, instructions, tools, workflows, and agent templates.

Artifacts retain provenance: which objective and run created them, what inputs were used, who approved them, and what superseded them.

### 12. Blazn Button

The Blazn Button connects an application or website to a workspace and its agents. It is the product-facing bridge for real-time work.

Potential experiences include:

- Starting an agent session from the context of the current screen or record.
- Showing live progress and results from a connected execution environment.
- Reporting an issue with relevant application context attached.
- Handling customer support with human and agent collaboration.
- Requesting analysis, remediation, content, or operational work without leaving the host product.

The Button inherits the interaction patterns and visual language of the existing Blaze Button while using Blazn's identity, policy, orchestration, and run history underneath.

The goal is to bring Blazn into the applications people already use. With the user's permission, the Button supplies relevant live context so an agent can understand what the person is seeing, join the experience in the moment, perform work in a connected environment, and return real-time progress and results without forcing the person to reconstruct that context elsewhere.

## System design

This section turns the product vision into a shared system model. It will be developed component by component, beginning with the execution fabric and then defining the resources scheduled onto it.

### System component index

| Component | Purpose | Design status |
| --- | --- | --- |
| [Nodes](#nodes) | Contribute, describe, protect, and operate compute capacity | Initial design |
| Sandbox templates and refreshes | Define reproducible environments and how their base state is updated | Planned |
| Sandboxes | Provide isolated, stateful or disposable execution environments | Planned |
| Warm pools | Keep policy-controlled environments ready to reduce startup latency | Planned |
| Analytics and events | Record the structured history of work and system activity | Planned |
| Metrics | Measure health, capacity, cost, performance, and outcomes | Planned |
| Queues | Admit and prioritize work across limited models and compute | Planned |
| Temporary agents | Create bounded, task-specific agent identities and lifetimes | Planned |
| Agents | Define durable agent identity, objectives, configuration, and history | Planned |
| Development | Build, test, version, evaluate, and release agents and system components | Planned |
| Blazn Agent Harness | Provide the canonical runtime for agent context, tools, execution, collaboration, and recovery | Initial design |
| Credentials and integrations | Connect external systems and safely grant scoped access | Planned |
| MCP | Expose Blazn resources and controls to agents and compatible clients | Planned |
| API | Provide the authoritative programmatic control surface | Planned |
| Smart LLM Router | Select, queue, load-balance, and fail over model requests across local and cloud capacity | Initial design |
| LLM Router Policy | Define allowed routes, preferences, budgets, privacy rules, and fallback behavior | Initial design |

### Nodes

#### Definition

A node is an enrolled machine that makes one or more capabilities available to a Blazn workspace. It may be a person's laptop or desktop, a company workstation, a dedicated server, or capacity managed by Blazn cloud.

Once a machine owner or administrator opts it in, Blazn can automatically create approved virtual environments or sandboxes on that machine and schedule compatible agent work into them. Enrollment does not grant agents unrestricted access to the host. The node contributes only the capabilities, resource envelope, time windows, and data access allowed by its policy.

The node is the unit of capacity and trust. A sandbox is a workload environment created on that capacity. Keeping these concepts separate allows Blazn to use different isolation technologies on macOS, Windows, Linux, and cloud infrastructure without changing how users describe or schedule agent work.

#### Why nodes matter

Nodes turn otherwise isolated machines into a governed compute fabric. A company can use available capacity across employee and company-owned hardware, increase concurrency without immediately purchasing cloud instances, and reserve managed cloud capacity for workloads that require elasticity, availability, or a different trust boundary.

This fabric also protects the local experience. Instead of launching an arbitrary number of agents directly on a workstation, Blazn admits work against declared limits, isolates it where possible, and moves or queues it when the machine cannot safely support more work.

#### Node types

- **Personal node:** A person's macOS, Windows, or Linux machine. It is interactive, may be intermittently available, and always prioritizes the owner's foreground work.
- **Shared team node:** A company-owned machine made available to one or more workspaces under centrally managed policy.
- **Dedicated worker:** A server or workstation intended primarily for agents, with higher concurrency and fewer interactive-use restrictions.
- **Blazn cloud node:** Managed capacity supplied on demand with a defined region, hardware profile, price, and security boundary.
- **Model node:** A machine that exposes local model inference capacity. It may also execute agent environments, but the two capacities are advertised and scheduled independently.

A physical machine may provide multiple execution backends. For example, a Mac could provide a Linux virtual-machine backend for isolated general workloads, a native macOS backend for Xcode work, and a local model endpoint. Each backend has separate limits and trust characteristics even though the UI groups them under one node.

#### Enrollment and identity

Enrollment begins in the desktop application or CLI and should require an explicit user or administrator action. The basic flow is:

1. Authenticate the person and select a workspace.
2. Name the node and show the capabilities Blazn detected.
3. Choose what the machine may contribute, when it is available, and how much capacity must remain reserved for its owner.
4. Apply a workspace-managed node policy and display any settings it controls.
5. Create a unique device identity and register its public identity with the workspace.
6. Establish an outbound authenticated connection to the Blazn control plane.
7. Run compatibility and isolation checks before marking any execution backend ready.

Node identity is distinct from user identity. Removing a user, rotating credentials, reinstalling the node service, or transferring machine ownership must not accidentally preserve access. Device credentials should be revocable, rotated automatically, stored in the platform credential store, and limited to the registered node and workspace.

#### Advertised capabilities

The node service continuously reports a normalized capability inventory that the scheduler can match against workload requirements:

- Operating system, version, CPU architecture, and execution backends.
- CPU, memory, storage, GPU or accelerator capacity, including the amount currently allocatable.
- Installed or managed runtimes, virtualization support, and sandbox providers.
- Native toolchains such as Xcode, Android SDKs, browsers, or Windows build tools.
- Local models and model-server compatibility, context limits, and current inference capacity.
- Network class, allowed destinations, region, data-residency attributes, and trust level.
- Maximum concurrency, availability schedule, battery and thermal restrictions, and owner-defined labels.
- Supported sandbox templates and cached environment versions.

Capabilities are claims, not guarantees. Blazn validates important capabilities during enrollment and refreshes health signals while the node is connected.

#### Local model capacity

A node can contribute one or more locally hosted models as shared inference capacity for authorized agents across the company. For example, a workstation or dedicated GPU server could host DeepSeek V4 Flash or Qwen3.8 and make that model available through the Blazn AI Proxy without requiring every employee or agent to install, configure, or address the model server directly.

Local model capacity is a first-class node backend, parallel to sandbox and native-execution backends. A node may offer models only, execution environments only, or both. The node advertises each model instance with:

- A stable workspace-facing model name and the underlying model, version, quantization, and runtime.
- Supported interfaces and capabilities, such as chat, tool use, structured output, embeddings, vision, streaming, and maximum context.
- Accelerator and memory requirements, loaded or unloaded state, warm-up time, concurrency, token throughput, and current queue depth.
- Data-handling attributes, network exposure, trust class, allowed workspaces or teams, and whether prompts may leave the node.
- Cost or capacity-accounting policy, availability schedule, fallback options, and owner-defined limits.

Agents request a model or capability through the AI Proxy rather than connecting to a machine by address. The proxy authenticates the caller, applies workspace policy and budgets, selects an eligible model instance, queues or load-balances the request, and records operational metrics. Requests reach the node through its authenticated Blazn connection, so contributing a local model does not require exposing its raw inference port to the company network or internet.

The workspace can publish a friendly model alias that maps to several eligible backends. An alias such as `company-fast` could prefer a local Qwen3.8 instance, fail over to another company node when it is saturated, and use an approved Blazn cloud model only when local capacity is unavailable and policy permits. The Blazn Agent Harness and authorized external clients keep using the same model name while routing changes behind it.

Model serving shares the physical node with the owner's applications and possibly with agent sandboxes. The node resource manager therefore reserves memory and accelerator capacity for loaded models, limits concurrent inference, coordinates model loading and eviction, and prevents sandbox admission from exhausting resources promised to inference. The machine owner or administrator can independently pause model serving, sandbox work, or the entire node.

Prompts, responses, and retrieved context remain workspace data. They are not included in infrastructure telemetry by default. Blazn records routing decisions, token counts, latency, errors, saturation, and cost or avoided-cost estimates, while content logging, retention, and evaluation require an explicit workspace policy.

Company-wide model sharing should provide three benefits:

- Turn existing local hardware and approved open models into reusable organizational capacity.
- Reduce dependence on per-user subscription limits and expensive API traffic for suitable workloads.
- Keep sensitive requests inside an approved company-controlled boundary when policy requires it.

#### Lifecycle and state

A node has a small, explicit lifecycle:

- **Enrolling:** Identity exists, but compatibility and policy checks are incomplete.
- **Ready:** At least one backend can accept work.
- **Busy:** The node is healthy but has reached an admission or resource limit.
- **Draining:** Existing work may finish or migrate, but no new work is admitted.
- **Offline:** The node has missed its lease or intentionally disconnected.
- **Quarantined:** Blazn or an administrator has prevented execution because identity, integrity, policy, or health checks failed.
- **Removed:** Trust is revoked and the machine is no longer part of the workspace.

Each execution backend and sandbox also reports its own state. A node can therefore remain ready for local-model requests while its sandbox backend is draining, or remain available for Linux work while native macOS execution is disabled.

#### Placement and admission

Agents request capabilities rather than choosing a machine by hostname. A request may specify an operating system, architecture, sandbox template, native toolchain, model, minimum resources, trust level, region, data boundary, expected duration, and whether interruption is allowed.

The scheduler filters nodes that cannot satisfy hard requirements, then selects among eligible capacity using:

- Workspace and node policy.
- Queue priority, quotas, and fairness.
- Current load and the owner's reserved resources.
- Environment or model cache locality.
- Data location and network restrictions.
- Startup latency, expected reliability, and interruption risk.
- User preference and estimated cost.

Users can pin work to a node for development or specialized hardware, but ordinary runs should remain portable. If no node is eligible, the request waits in a queue, asks the user to relax a constraint, or—when policy permits—offers Blazn cloud capacity with the expected cost visible before admission.

#### Automatic environment provisioning

When the scheduler admits work, the selected node receives a signed workload grant rather than a general command channel. The node then:

1. Resolves an approved sandbox template and exact version.
2. Reuses a compatible warm environment or creates a fresh one.
3. Applies resource, network, filesystem, tool, and credential policy.
4. Starts the Blazn Agent Harness worker and requested agent inside the chosen execution backend.
5. Streams lifecycle events, logs, metrics, and selected artifacts.
6. Suspends, refreshes, or destroys the environment according to its retention policy.

Template refresh, warm-pool behavior, sandbox identity, persistence, and cleanup will be defined in their dedicated sections. The node is responsible for faithfully enforcing those decisions, not inventing them locally.

#### Protecting the machine owner

Personal nodes must remain safe and usable while they contribute capacity. The node service enforces:

- Hard CPU, memory, storage, process, and concurrency limits.
- A configurable reserve that agent workloads cannot consume.
- Low-disk, memory-pressure, thermal, battery, and foreground-activity thresholds.
- Availability windows and idle-only operation when requested.
- Immediate pause, drain, and stop controls in the desktop app and CLI.
- Preemption of interruptible agent work when the owner needs resources.
- Bounded log, cache, image, and sandbox storage with visible cleanup controls.
- Crash recovery that detects and cleans up orphaned environments without restarting the host.

The default personal-node policy should be conservative. Increasing capacity is an informed user choice; joining a workspace should never silently turn a machine into an unrestricted worker.

#### Security model

Nodes operate on the assumption that agent code, repositories, tools, and model output may be untrusted.

- The node initiates outbound connections; enrollment should not require exposing a general-purpose inbound management port.
- Every workload receives a short-lived, audience-bound grant scoped to one run and execution backend.
- Sandbox images and templates are signed, versioned, and policy-approved.
- Host files, sockets, devices, clipboard data, and credentials are unavailable unless explicitly attached.
- Secrets are delivered just in time, scoped to the run and integration, redacted from telemetry, and revoked when possible at completion.
- Network access is denied or constrained by template and workspace policy.
- Native-host execution is identified as a higher-trust backend and requires stronger approval and narrower workloads than a sandboxed backend.
- Node actions and administrative changes produce immutable audit events.

Workspace policy must be able to prohibit personal nodes, require company-managed devices, restrict data to a region or trust class, or allow only specific repositories and integrations.

#### Reliability and disconnection

The node maintains a renewable lease with the control plane. When the lease expires, no new work is assigned. The control plane distinguishes a disconnected node from a failed run and applies the workload's recovery policy:

- Wait for the same node when it owns irreplaceable local state.
- Resume a persistent sandbox after reconnection.
- Retry portable work on another eligible node.
- Fail and request human intervention when replay could duplicate an external action.

The node journals enough local state to report what happened after reconnecting. The control plane remains authoritative for run intent, while the node remains authoritative for the observed state of its local environments until reconciliation completes.

#### Observability and privacy

The workspace needs enough information to schedule work and diagnose failures without turning node enrollment into employee surveillance. Node telemetry should include:

- Availability, heartbeat, software version, and backend health.
- Allocatable and consumed CPU, memory, storage, accelerator, and concurrency capacity.
- Sandbox startup time, queue-to-start time, failures, preemptions, and cleanup results.
- Model capacity, request load, latency, and error rates when the node serves models.
- Workload identifiers and policy decisions required for audit and support.

Blazn should not collect unrelated applications, personal files, keystrokes, screen contents, or browsing activity. Application context is shared only through an explicit integration such as the Blazn Button and is governed separately from infrastructure telemetry.

#### Node controls and experience

The desktop app should make node behavior understandable at a glance:

- Whether the node is accepting work and which backends are ready.
- What is running, for whom, and what resources it is using.
- The capacity reserved for the owner and the capacity contributed to Blazn.
- Which local models are available, loading, serving, saturated, or offline.
- Recent runs, errors, policy changes, and cleanup activity.
- Pause, resume, drain, update, diagnose, and remove actions.
- Estimated local capacity contributed and cloud cost avoided.

Administrators need fleet views for health, versions, capacity, utilization, trust, queues, policy compliance, and current workloads. Machine owners should always retain the local ability to stop Blazn work, even when workspace policy is centrally managed.

#### Initial node record

The first system model should include at least:

- Stable node ID, workspace ID, display name, owner, and enrollment timestamps.
- Device identity, trust class, policy version, and software version.
- Platform and normalized capabilities.
- Execution backends with individual status and allocatable capacity.
- Advertised model instances, aliases, runtime details, access policy, and inference capacity.
- Labels, placement constraints, availability, and resource-reserve policy.
- Current lease, health, drain state, and last-seen time.
- Active environment and workload references.
- Aggregate utilization and reliability metrics.

Secrets, raw device keys, and unrelated host inventory must not be stored in the node record.

#### Version-one boundary

The first node implementation should prove a narrow loop:

1. Enroll one macOS, Windows, or Linux machine from the app or CLI.
2. Contribute a bounded Linux virtualized or containerized backend where the platform supports it.
3. Advertise verified resources and accept capability-matched work.
4. Advertise one local model endpoint and make it available to authorized agents through the AI Proxy.
5. Create an isolated environment from one approved template.
6. Run the Blazn Agent Harness, stream events and resource metrics, and return artifacts.
7. Enforce shared resource reserves, model and agent concurrency, drain, offline, and cleanup behavior.
8. Make the same operations available through the authenticated Blazn API and MCP server.

Native platform execution, additional model runtimes, organization-wide device management, workload migration, and cloud bursting belong in the model from the beginning but can follow after this loop is reliable.

#### Decisions to make next

- Which isolation backend is the default on each operating system?
- Does the first release use a local scheduler, a hosted control plane, or both?
- Which node and sandbox operations continue while the control plane is unavailable?
- How are software, template, and policy updates staged and rolled back?
- What data and workload classes are permitted on personal versus managed nodes?
- How is contributed capacity measured, credited, budgeted, or billed?
- How are local model aliases, compatibility claims, model versions, and fallback chains governed?
- How should inference and agent environments share accelerators and memory without destabilizing the node?
- Which workload types are portable, resumable, interruptible, or bound to one node?
- What compatibility contract allows Kubernetes Agent Sandbox and non-Kubernetes backends to behave consistently?

### Blazn Agent Harness system

#### Definition and authority

The Blazn Agent Harness is the authoritative runtime for every Blazn-managed agent. It converts an objective, conversation, schedule, trigger, API call, or delegated assignment into a durable run and coordinates that run until it reaches a terminal or explicitly suspended state.

Blazn owns the harness contract. This provides one consistent model for identity, context, tools, approvals, environments, events, artifacts, metrics, recovery, and improvement across desktop, CLI, cloud, and embedded experiences. Supporting an external tool or model API does not mean delegating control of the agent lifecycle to an external harness.

#### Harness responsibilities

For each run, the harness is responsible for:

- Resolving the agent version, objective, instructions, skills, permissions, and effective LLM Router Policy.
- Building a bounded context from the current conversation, workspace memory, project state, pinned resources, prior run evidence, and explicit user input.
- Acquiring a sandbox or approved native execution backend with the required capabilities.
- Making tools, MCP servers, integrations, and credentials available according to least-privilege grants.
- Sending all inference requests through the Smart LLM Router.
- Executing the agent loop and recording model responses, reasoning summaries where supported, tool requests, approvals, results, and errors as structured events.
- Accepting live user messages, steering, cancellation, and approval decisions without losing run identity.
- Creating temporary agents or delegating bounded objectives when the parent agent is permitted to do so.
- Producing artifacts, a final result, outcome metrics, and post-run introspection.
- Releasing or retaining environments and credentials according to policy.

#### Run and session model

A session is the durable interaction boundary between people and one or more agents. A run is one execution attempt within that session. Follow-up instructions may continue the current run when it is still active or create a new run linked to the same session and prior evidence.

The initial run lifecycle should include:

- **Created:** Intent is recorded but has not entered admission.
- **Queued:** The run is waiting for agent, model, environment, budget, or approval capacity.
- **Preparing:** Context, credentials, tools, and an execution environment are being resolved.
- **Running:** The harness is actively executing the agent loop.
- **Waiting:** The run is waiting for a person, dependency, scheduled time, or external event.
- **Suspended:** State is checkpointed and resources may be released.
- **Completing:** Outputs, artifacts, evaluations, and cleanup are being finalized.
- **Succeeded, failed, or canceled:** The terminal outcome and reason are recorded.

Run state is durable in the control plane. A desktop application closing, a client disconnecting, or an execution worker restarting must not erase the run or make its outcome unknowable.

#### Context and memory

The harness assembles context intentionally rather than placing the entire company brain into every prompt. Context sources are permission-filtered, ranked for the objective, budgeted against the selected model's context window, and recorded by reference for provenance.

The harness distinguishes:

- User-authored instructions and current conversation.
- Agent identity, role, skills, and versioned operating instructions.
- Project objectives, tasks, decisions, and current status.
- Retrieved workspace knowledge and pinned resources.
- Environment observations and tool results from the current run.
- Summaries and evaluated evidence from prior runs.

Important outputs return to the workspace as versioned artifacts or proposed memories. Raw model output does not silently become trusted company knowledge.

#### Tools, actions, and approvals

The harness exposes tools through a normalized contract regardless of whether they are native Blazn tools, MCP tools, workspace integrations, or environment operations. Each call carries the run and agent identity, a scoped authorization grant, a deadline, and an idempotency or replay policy.

Read-only and reversible actions may run automatically under policy. External writes, financial actions, production changes, secrets access, destructive operations, and actions affecting other people can require explicit approval. If execution fails after an action may have occurred, the harness must reconcile the result rather than blindly repeating it.

#### Temporary agents and delegation

A durable agent may create a temporary agent for a bounded sub-objective. The temporary agent inherits only explicitly delegated context, tools, credentials, budget, routing policy, environment requirements, and deadline. It has its own run identity and event stream while remaining linked to its parent.

The parent agent or coordinating agent is responsible for evaluating and integrating delegated results. Temporary agents expire after their assignment and do not silently become permanent members of the workspace or retain credentials.

#### Checkpointing and recovery

Checkpoints capture the durable information required to resume safely: conversation state, completed tool actions, pending approvals, selected artifacts, environment references, routing history, budgets, and the next intended step. Checkpoints do not assume that a model's hidden internal state can be transferred between providers.

After a harness, node, or model failure, the reconciler determines whether to resume, retry a pure operation, select another model, move portable work, wait for the original environment, or request human intervention. Recovery follows recorded policy and idempotency rules.

#### External clients

Codex, Claude Code, IDEs, and other applications can use the Blazn AI Proxy, API, MCP server, tools, and artifacts. They remain external clients with their own execution behavior. When a user creates an agent in Blazn, however, that agent runs through the Blazn Agent Harness so the workspace receives consistent controls, events, metrics, and recovery semantics.

#### Version-one boundary

The first harness should prove one durable end-to-end loop:

1. Start a session from desktop, CLI, API, or MCP.
2. Resolve one versioned agent and its effective policy.
3. Acquire one sandbox, assemble bounded context, and attach approved tools.
4. Route all model requests through the Smart LLM Router.
5. Stream events and accept a follow-up message, cancellation, or approval.
6. Survive a harness process restart without losing the run.
7. Produce a final result, artifacts, metrics, and cleanup outcome.

Multi-agent delegation, advanced introspection, long-lived suspension, and broader tool compatibility can build on this contract after the single-agent loop is reliable.

### Smart LLM Router

#### Definition

The Smart LLM Router is the decision and traffic layer inside the Blazn AI Proxy. It receives model requests from the Blazn Agent Harness or an authorized external client, evaluates the effective LLM Router Policy, selects an eligible model instance, manages queueing and fallback, and returns a normalized response.

The router separates an agent's need from a provider-specific model name. A request may ask for a logical alias such as `company-fast`, `coding-best`, or `private-reasoning`, or describe required capabilities such as tool use, vision, structured output, minimum context, latency class, and quality tier.

#### Request contract

A routed request should include:

- Workspace, principal, agent, run, and session identity where applicable.
- Logical model alias or required capabilities.
- Input messages and estimated context size.
- Required modalities, tool-calling behavior, structured-output schema, and streaming mode.
- Data classification, residency, retention, and provider restrictions.
- Latency target, priority, deadline, token limit, and remaining budget.
- Retry and fallback safety markers.
- Optional preference for local, company-managed, provider, or Blazn cloud capacity.

External compatibility endpoints derive this metadata from the caller's credential, selected alias, headers, and workspace defaults. Missing metadata never bypasses policy.

#### Selection pipeline

For each request, the router:

1. Authenticates the caller and resolves the effective policy.
2. Rejects destinations that violate security, privacy, residency, capability, or budget constraints.
3. Discovers healthy model instances across local nodes, company infrastructure, approved providers, and Blazn cloud.
4. Scores eligible destinations using capability fit, measured quality, availability, queue time, latency, cost, cache locality, and policy preference.
5. Reserves inference capacity or places the request in the appropriate model queue.
6. Sends the request through a provider adapter and normalizes streaming events, usage, tool calls, structured output, and errors.
7. Evaluates retry or fallback rules when the selected route cannot complete.
8. Records the routing decision and outcome without retaining prompt content unless policy allows it.

Hard policy constraints always outrank optimization. The router does not send restricted data to a cheaper or faster destination that is not permitted.

#### Aliases and route pools

A model alias points to a policy-controlled pool, not a single endpoint. For example, `company-fast` might contain local Qwen3.8 instances on several nodes, followed by a compatible Blazn cloud model. `private-reasoning` might allow only models running on company-managed nodes and prohibit external fallback entirely.

Aliases allow agents and external clients to remain stable while administrators add hardware, update a model, change providers, or alter preferences. Exact model pinning remains available for evaluation, reproducibility, and specialized workloads but is subject to the same hard policy constraints.

#### Fallback behavior

Fallback is a policy decision with an explicit reason, not an unconditional retry list. It may be triggered by unavailability, saturation, timeout, rate limiting, context overflow, unsupported capabilities, provider error, quality-gate failure, or exhausted local capacity.

Before falling back, the router considers:

- Whether the request may be safely replayed.
- Whether a partial streamed response has already been exposed.
- Whether the next destination supports the same modalities, tools, schema, and context.
- Whether moving the request changes its privacy, residency, retention, or cost boundary.
- Whether the policy prefers waiting locally over paying for cloud capacity.
- Whether the run's deadline and remaining budget allow another attempt.

The router attaches every attempt to one logical request so the harness and analytics system can distinguish retries from new model turns. It never retries tool side effects; the harness owns tool execution and reconciliation.

#### Queueing and capacity

Inference has its own queues and admission controls because model capacity may be scarce even when agent execution capacity is available. The router accounts for per-model concurrency, tokens in flight, accelerator memory, warm-up time, provider rate limits, team quotas, priority, and fairness.

A policy can choose among waiting, using another local node, switching to a compatible smaller model, using a paid provider, or failing fast. The user and harness should receive queue position or expected delay when it is meaningful.

#### Routing intelligence

The first router should be deterministic and policy-led. Over time, measured results can improve scoring using task class, model quality, successful tool use, latency, failure rate, and cost. Learned routing may rank options only within the destinations already permitted by policy, and its reasoning must remain inspectable and reversible.

#### Observability

For every logical request, Blazn records:

- Effective policy and alias version.
- Eligible and rejected route classes with reason codes.
- Selected model, runtime, node or provider, and selection reason.
- Queue time, attempts, retries, fallbacks, latency, tokens, and cost.
- Whether data remained local, stayed on company infrastructure, or used an external provider.
- Errors, cancellation, saturation, and normalized completion status.
- Quality or outcome measurements linked later by the harness.

Prompt and response content are governed separately from operational routing telemetry.

#### Version-one boundary

The first Smart LLM Router should support:

- One OpenAI-compatible entrypoint used by the Blazn Agent Harness and authorized external clients.
- Logical aliases mapped to a local model, one approved provider, and one Blazn cloud route.
- Capability and policy filtering before selection.
- Health-aware routing, queueing, timeout, and ordered fallback.
- Streaming and non-streaming responses with normalized errors and usage.
- An auditable explanation for every route and fallback.

### LLM Router Policy

#### Definition

An LLM Router Policy is a versioned workspace resource that determines which models may receive a request, which routes are preferred, how long Blazn should wait, and what fallback behavior is allowed. It separates routing governance from agent instructions and application code.

Policies can be attached at several scopes:

```text
Organization constraints
  -> Workspace defaults
    -> Team or project policy
      -> Agent policy
        -> Run request
```

More specific scopes can choose among allowed behavior but cannot weaken a higher-level security, privacy, residency, provider, retention, or budget restriction. The effective policy and all source versions are captured on the run and each routed request.

#### Policy contents

An LLM Router Policy should be able to define:

- Allowed and prohibited models, aliases, providers, node trust classes, and regions.
- Required capabilities, minimum context, modalities, tool use, and structured-output support.
- Local-first, company-only, cloud-first, cloud-disabled, or balanced routing preferences.
- Data classifications and the destinations permitted to process each class.
- Prompt, response, and provider-retention rules.
- Maximum tokens, request cost, run budget, daily budget, and concurrency.
- Queue wait limits, latency targets, deadlines, provider rate limits, and fairness class.
- Primary route pools, fallback chains, retry limits, timeouts, and fail-closed behavior.
- Whether a boundary-changing fallback requires user approval.
- Quality tiers, evaluation thresholds, and exact-model requirements.
- Whether routing intelligence may use historical performance to rank eligible destinations.

#### Example policy

```yaml
name: company-fast
version: 1
requirements:
  tools: true
  data_classification: internal
routing:
  preference: local-first
  primary:
    alias: local-qwen
  wait_for_local: 20s
  fallback:
    - alias: company-qwen-pool
    - alias: blazn-cloud-fast
limits:
  max_cost_usd_per_request: 0.10
  max_attempts: 3
privacy:
  external_providers: denied
  content_logging: denied
on_no_compliant_route: fail
```

This example prefers a local model, waits briefly when it is saturated, tries another company-controlled instance, and then uses a permitted Blazn cloud route. If no route satisfies the privacy and cost rules, the request fails clearly rather than escaping policy.

#### Evaluation and changes

Policies are validated before activation. Blazn should detect aliases with no eligible destinations, impossible capability combinations, fallback cycles, routes that exceed their budget, and privacy rules contradicted by a provider's retention behavior.

Policy changes are versioned, auditable, previewable against recent traffic, and reversible. Administrators should be able to simulate how a proposed policy would route representative requests before applying it. Active runs retain their captured version unless an emergency organization policy revokes a destination.

#### Version-one boundary

The first policy model should include:

- Workspace defaults with optional agent-level specialization.
- Allowed destinations and logical aliases.
- Local-first or cloud-first preference.
- Data-boundary and content-retention restrictions.
- Queue timeout, maximum attempts, and ordered fallback.
- Per-request and per-run cost limits.
- Fail-closed behavior and optional approval before cloud fallback.
- Versioning, validation, audit history, and route simulation.

## How the pieces fit

```mermaid
flowchart LR
    People[People and teams] --> Clients[Desktop app / CLI / Blazn Button]
    Products[Connected products] --> Clients
    Clients --> Workspace[Blazn workspace and company brain]
    Workspace --> Orchestration[Agents, projects, runs, schedules and events]
    Orchestration --> Harness[Blazn Agent Harness]
    Harness --> Router[Smart LLM Router / AI Proxy]
    Harness --> Execution[Execution fabric]
    Policy[LLM Router Policy] --> Router
    Router --> Models[Local, provider and Blazn cloud models]
    Execution --> Nodes[User and team nodes]
    Execution --> Sandboxes[Sandboxes and virtual environments]
    Execution --> Cloud[Blazn cloud capacity]
    Harness --> Memory[Artifacts, analytics and improvement]
    Memory --> Workspace
```

## Product principles

### Local-first, cloud-optional

One person and one machine should be enough to get value. Cloud services add collaboration, reach, and capacity without taking ownership away from the user.

### People remain in control

Users can see what agents are doing, interrupt work, approve sensitive actions, set boundaries, and understand outcomes.

### One harness, many models and tools

Blazn should provide one consistent agent runtime across different model providers, tools, MCP servers, local and cloud environments, and product surfaces. External developer tools can use Blazn services, but Blazn-managed agents retain one observable and recoverable lifecycle.

### Work creates memory

Important context, decisions, evidence, and artifacts should become reusable workspace knowledge with provenance and permissions.

### Improvement is observable and governed

Agents learn through measured outcomes and explicit, versioned changes. Improvement must remain reviewable, reversible, and aligned with workspace policy.

### Secure by default

Isolation, identity, least privilege, secrets handling, auditability, and data boundaries are product requirements, not deployment options.

### Progressive complexity

Simple tasks should feel simple. Infrastructure, routing, queues, and orchestration details should become visible only when a user needs control over them.

## Brand direction

Blazn belongs to the existing Blaze product family and should reuse the visual system established by Blaze Proxy and Blaze Button:

- Dark, focused surfaces led by a `#101010` canvas and `#171717` panels.
- Blaze orange as the primary signal color, using the established orange/red range (`#f97316` and `#ff2e00`).
- Clear neutral typography with restrained status colors.
- Rounded, compact application surfaces and the existing flame language for iconography.
- A technical, fast, confident tone without making the product feel inaccessible.

The visual identity should evolve as one system across desktop, CLI output, web surfaces, embedded Button experiences, documentation, and cloud administration. Existing Blaze assets are references; final naming, trademarks, and asset licensing should be confirmed before public release.

## Initial product boundary

The first version should prove the core loop:

> Create a workspace, configure an agent, choose a model, run useful work in a controlled environment, follow and steer it, and preserve the outcome for the team.

It does not need to deliver the full company-brain vision on day one. The early product can focus on:

- A single-user workspace with a clear path to team collaboration.
- Desktop and CLI access to the same local control plane.
- A small agent library running through the Blazn Agent Harness.
- Smart LLM Router access to a local model and selected cloud providers.
- A versioned LLM Router Policy defining allowed routes, budgets, queueing, and fallback.
- One contributed machine acting as a worker.
- Isolated Linux execution for an initial class of workloads.
- Runs with live events, logs, artifacts, and basic metrics.
- A minimal project/task connection.
- Secure remote control through an MCP-compatible interface.

Native macOS and Windows execution, broad multi-agent coordination, advanced organizational memory, autonomous improvement, elastic cloud capacity, and the full Blazn Button platform can then be introduced in deliberate stages.

## What Blazn is not

- It is not only another chat interface.
- It is not only a model gateway.
- It is not only a Kubernetes or sandbox manager.
- It is not a replacement for every specialized IDE, model provider, or project tool.
- It does not give agents unrestricted access to a user's machines or company data.
- It does not treat unreviewed self-modification as learning.

Blazn is the connective operating layer that makes these capabilities work together as a trusted agent team.

## Measures of success

At a high level, Blazn succeeds when:

- A new user completes useful agent work quickly on their existing machine.
- A team can find and reuse prior work instead of recreating context.
- Agents complete more objectives with fewer failed runs and less human coordination.
- Users can understand and control model routing, execution, cost, permissions, and data location.
- Adding a machine or managed capacity increases throughput without increasing operational burden.
- Agent and team improvements are supported by measurable run evidence.
- Product and customer interactions initiated through the Blazn Button become traceable work with timely outcomes.

## Documents to develop next

This overview establishes the product direction. Follow-on documents should define:

1. Target users, jobs to be done, and initial launch persona.
2. Version-one scope and end-to-end user journeys.
3. Product architecture and trust boundaries.
4. Workspace, agent, run, artifact, project, and event data models.
5. Node enrollment, sandboxing, native execution, and scheduling model.
6. AI Proxy compatibility, Smart LLM Router architecture, policy evaluation, and provider strategy.
7. MCP and public API surface, authentication, and authorization.
8. Company-brain memory, retrieval, provenance, retention, and privacy model.
9. Agent evaluation, introspection, and governed improvement process.
10. Blazn Button SDK and embedded interaction model.
11. Desktop/CLI technology choices and release strategy.
12. Commercial model for local, team, and cloud offerings.

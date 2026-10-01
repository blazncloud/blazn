import assert from "node:assert/strict";
import test from "node:test";
import { AgentRunControllerService } from "./agent-run-controller-service.js";
import type { AgentRunControllerStore, AgentRunWorkItem } from "./agent-run-controller-store.js";
import type { AgentRunExecution, AgentRunExecutionStore, AgentRunLease, AgentRunMessageClaim } from "./agent-run-execution-store.js";
import { AgentRunExecutor, RunLeaseLost, RunRetryable, type AgentRunExecutorOptions } from "./agent-run-executor.js";
import { ModelRouter, parseModelRoutes, type ModelRoute } from "./agent-run-model.js";
import { SandboxAccessError, type SandboxAccess, type SandboxExecResult } from "./agent-run-sandbox-access.js";

const runId = "50000000-0000-4000-8000-000000000001", routeId = "60000000-0000-4000-8000-000000000001", nodeId = "70000000-0000-4000-8000-000000000001";
const sandboxId = "80000000-0000-4000-8000-000000000001", messageId = "90000000-0000-4000-8000-000000000001", claimId = "90000000-0000-4000-8000-000000000002";
const directory = "/workspace/artifacts/blazn-agent";
const artifactIds = { patch: "f0000000-0000-4000-8000-000000000001", summary: "f0000000-0000-4000-8000-000000000002" };

function item(): AgentRunWorkItem {
  const digest = `sha256:${"a".repeat(64)}`;
  return { runId, workspaceId: "10000000-0000-4000-8000-000000000001", projectId: "20000000-0000-4000-8000-000000000001", runVersion: 1, leaseToken: "30000000-0000-4000-8000-000000000001",
    leaseExpiresAt: "2026-10-01T00:00:00Z", attempt: 1, requestedBy: "40000000-0000-4000-8000-000000000001", planDigest: digest, agentVersionId: "a0000000-0000-4000-8000-000000000001",
    agentVersionDigest: digest, agentVersion: {}, harnessDefinitionId: "b0000000-0000-4000-8000-000000000001", harnessVersionId: "c0000000-0000-4000-8000-000000000001", harnessVersionDigest: digest,
    harnessVersion: {}, harnessProfileId: "d0000000-0000-4000-8000-000000000001", harnessProfileDigest: digest, harnessProfile: {}, templateVersionId: "e0000000-0000-4000-8000-000000000001",
    templateDigest: digest, modelRouteId: routeId, modelRouteVersion: 1, modelProtocol: "openai-chat" };
}

/** Records controller calls and plays the Run's database state. */
class FakeStores implements AgentRunExecutionStore, AgentRunControllerStore {
  state: AgentRunExecution = { sandboxId, sandboxState: "provisioning", sandboxExpiresAt: new Date(Date.now() + 3600_000).toISOString(), architecture: "amd64",
    outboxSequence: 0, runStatus: "queued", runVersion: 1, instructions: "Be helpful.", purpose: "test", repositoryDestination: "/workspace/src/blazn" };
  leaseHeld = true;
  queue: AgentRunMessageClaim[] = [{ messageId, claimId, ordinal: 1, kind: "prompt", content: "Create a proof file", resumed: false }];
  claimed: AgentRunMessageClaim | undefined;
  replies: { sequence: number; parent?: string; content: string }[] = [];
  events: { sequence: number; type: string | null; payload: Record<string, unknown> }[] = [];
  delivered: string[] = [];
  artifacts = new Map<string, Buffer>();
  finalized?: { outcome: string; errorCode?: string; artifactIds: string[]; steps: number };
  bound = 0;

  async execution() { return this.leaseHeld ? { ...this.state } : undefined; }
  async issueGrant() { return this.leaseHeld; }
  async claimMessage() { if (!this.leaseHeld) return undefined; if (this.claimed) return { ...this.claimed, resumed: true }; this.claimed = this.queue.shift(); return this.claimed; }
  async deliverMessage(_lease: AgentRunLease, id: string) { this.delivered.push(id); this.claimed = undefined; return this.leaseHeld; }
  async recordReply(_lease: AgentRunLease, sequence: number, parent: string | undefined, content: string) {
    if (!this.leaseHeld) return undefined;
    if (!this.replies.some((reply) => reply.sequence === sequence)) this.replies.push(parent === undefined ? { sequence, content } : { sequence, parent, content });
    return `reply-${sequence}`;
  }
  async recordEvent(_lease: AgentRunLease, sequence: number, type: string | null, payload: Record<string, unknown>) {
    if (!this.leaseHeld) return false;
    if (sequence > this.state.outboxSequence) { this.events.push({ sequence, type, payload }); this.state.outboxSequence = sequence; }
    return true;
  }
  async recordArtifact(_lease: AgentRunLease, name: "patch" | "summary", content: Buffer) { this.artifacts.set(name, content); return artifactIds[name]; }
  async releaseSandboxes() { return 0; }

  async enqueue() { return true; }
  async claim() { return undefined; }
  async renew() { return this.leaseHeld ? new Date().toISOString() : undefined; }
  async bindSandbox(_run: string, _worker: string, _token: string, version: number, node: string, sandbox: string) {
    assert.equal(version, this.state.runVersion); assert.equal(node, nodeId); assert.equal(sandbox, sandboxId);
    this.bound++; this.state.runStatus = "running"; this.state.runVersion++; return true;
  }
  async retry() { return "retry_scheduled" as const; }
  async finalize(_run: string, _worker: string, _token: string, version: number, outcome: "succeeded" | "failed", errorCode: string | undefined, artifactIds: string[], steps: number) {
    assert.equal(version, this.state.runVersion);
    this.finalized = errorCode === undefined ? { outcome, artifactIds, steps } : { outcome, errorCode, artifactIds, steps };
    this.state.runStatus = outcome; return true;
  }
}

/** A Sandbox whose harness answers through the same file protocol as the real one. */
class FakeSandbox implements SandboxAccess {
  commands: string[][] = [];
  uploads = new Map<string, Buffer>();
  outbox: { seq: number; type: string; data: Record<string, unknown> }[] = [];
  alive = false;
  imageHasAgent = true;
  failExec = 0;
  private emit(type: string, data: Record<string, unknown>) { this.outbox.push({ seq: this.outbox.length + 1, type, data }); }

  async exec(_lease: AgentRunLease, command: string[]): Promise<SandboxExecResult> {
    this.commands.push(command);
    const ok = (stdout = ""): SandboxExecResult => ({ exitCode: 0, stdout: Buffer.from(stdout), stderr: Buffer.alloc(0), truncated: false });
    const bad: SandboxExecResult = { exitCode: 127, stdout: Buffer.alloc(0), stderr: Buffer.from("not found"), truncated: false };
    if (this.failExec > 0) { this.failExec--; throw new SandboxAccessError("access_failed", "transient"); }
    if (command[1] === "version") return command[0] === "/opt/blazn/agent" ? (this.imageHasAgent ? ok() : bad) : (this.uploads.has(command[0]!) ? ok() : bad);
    if (command[0] === "/bin/sh") return ok();
    if (command[1] === "init") return ok();
    if (command[1] === "start") { if (!this.alive) { this.alive = true; this.emit("started", { version: "0.1.0" }); } return ok(); }
    if (command[1] === "wait") {
      const after = Number(command[command.indexOf("--after") + 1]);
      const lines = this.outbox.filter((event) => event.seq > after).map((event) => JSON.stringify(event));
      lines.push(JSON.stringify({ type: "wait.status", alive: this.alive, lastSeq: this.outbox.length, more: false }));
      return ok(lines.join("\n") + "\n");
    }
    return bad;
  }

  async upload(_lease: AgentRunLease, path: string, content: Buffer) {
    this.uploads.set(path, content);
    if (!path.startsWith(`${directory}/inbox/`)) return;
    const value = JSON.parse(content.toString("utf8")) as { type: string; id?: string; requestId?: string; response?: { choices: { message: { content: string | null; tool_calls?: unknown[] } }[] }; error?: { code: string } };
    if (value.type === "message") {
      this.emit("message.accepted", { messageId: value.id });
      this.emit("model.request", { requestId: "m1", request: { model: "ignored", messages: [{ role: "system", content: "s" }, { role: "user", content: "Create a proof file" }], tools: [{ type: "function" }], tool_choice: "auto" } });
    } else if (value.type === "model.response" && value.requestId === "m1") {
      if (value.error) { this.emit("turn.completed", { messageId, status: "failed", errorCode: value.error.code, steps: 1 }); return; }
      assert.ok(value.response?.choices[0]?.message.tool_calls, "the stub model asks for a command first");
      this.emit("tool.started", { callId: "c1", name: "run_command", summary: "printf" });
      this.emit("tool.finished", { callId: "c1", name: "run_command", ok: true, exitCode: 0, truncated: false, outputPreview: "secret-looking output is not recorded" });
      this.emit("model.request", { requestId: "m2", request: { model: "ignored", messages: [{ role: "user", content: "x" }, { role: "tool", content: "blazn stub model saw: Create a proof file\n[exit code 0]" }] } });
    } else if (value.type === "model.response" && value.requestId === "m2") {
      this.emit("assistant.message", { messageId, content: value.response!.choices[0]!.message.content });
      this.emit("turn.completed", { messageId, status: "completed", steps: 2 });
    } else if (value.type === "finalize") {
      this.emit("finalized", { artifacts: [{ name: "patch", path: `${directory}/out/patch.diff` }, { name: "summary", path: `${directory}/out/summary.md` }], steps: 2, turns: 1, warnings: [] });
      this.alive = false;
    }
  }

  async download(_lease: AgentRunLease, path: string) { return Buffer.from(path.endsWith("patch.diff") ? "diff --git a/BLAZN_AGENT_PROOF.md b/BLAZN_AGENT_PROOF.md\n" : "# Run summary\n"); }
}

function build(stores: FakeStores, sandbox: FakeSandbox, routes: ModelRoute[], overrides: Partial<AgentRunExecutorOptions> = {}) {
  let clock = Date.now();
  const options: AgentRunExecutorOptions = {
    workerId: "worker-1", agentDirectory: directory, imageAgentPath: "/opt/blazn/agent", uploadedAgentPath: "/workspace/artifacts/blazn-agent-bin",
    harnessBinary: async () => Buffer.from("binary"), sandboxReadySeconds: 60, nodeObservationSeconds: 20, idleSeconds: 5, waitSeconds: 1, messageLeaseSeconds: 300, expiryMarginSeconds: 60,
    maxStepsPerTurn: 10, commandTimeoutSeconds: 30, now: () => clock, sleep: async (milliseconds) => { clock += milliseconds; }, ...overrides,
  };
  return new AgentRunExecutor(new AgentRunControllerService(stores), stores, sandbox, new ModelRouter(routes), options);
}
const stubRoute = parseModelRoutes(JSON.stringify({ routes: [{ id: routeId, version: 1, kind: "stub", model: "stub-model" }] }));

test("an Agent Run binds its Sandbox, relays a turn through the model, and finalizes with its artifacts", async () => {
  const stores = new FakeStores(), sandbox = new FakeSandbox();
  // The Sandbox becomes ready, then its Node is observed, while the executor polls.
  let polls = 0;
  const original = stores.execution.bind(stores);
  stores.execution = async () => { polls++; if (polls === 2) stores.state.sandboxState = "ready"; if (polls === 3) stores.state.nodeId = nodeId; return original(); };
  await build(stores, sandbox, stubRoute).execute(item(), new AbortController().signal);

  assert.equal(stores.bound, 1);
  assert.deepEqual(stores.replies, [{ sequence: 7, parent: messageId, content: 'Stub model: the sandbox command printed "blazn stub model saw: Create a proof file".' }]);
  assert.deepEqual(stores.delivered, [messageId]);
  assert.deepEqual(stores.finalized, { outcome: "succeeded", artifactIds: [artifactIds.patch, artifactIds.summary], steps: 2 });
  assert.ok(stores.artifacts.get("patch")!.toString().includes("BLAZN_AGENT_PROOF.md"));
  assert.deepEqual(stores.events.map((event) => event.type), ["harness-started", "turn-started", "model-request", "tool-started", "tool-finished", "model-request", null, "turn-completed", "harness-finalized"]);
  const modelEvent = stores.events.find((event) => event.type === "model-request")!;
  assert.deepEqual({ routeId: modelEvent.payload.routeId, model: modelEvent.payload.model, ok: modelEvent.payload.ok }, { routeId, model: "stub-model", ok: true });
  assert.ok(!JSON.stringify(stores.events).includes("secret-looking"), "tool output must not be copied into Run events");
  const config = JSON.parse(sandbox.uploads.get(`${directory}/config.json`)!.toString());
  assert.deepEqual({ runId: config.runId, model: config.model, workingDirectory: config.workingDirectory, repositoryDirectory: config.repositoryDirectory, instructions: config.instructions },
    { runId, model: "stub-model", workingDirectory: "/workspace/src/blazn", repositoryDirectory: "/workspace/src/blazn", instructions: "Be helpful." });
  assert.ok(!JSON.stringify(config).toLowerCase().includes("key"), "the harness configuration carries no credential");
  const inboxNames = [...sandbox.uploads.keys()].filter((path) => path.startsWith(`${directory}/inbox/`)).map((path) => path.slice(directory.length + 7));
  assert.ok(inboxNames.every((name) => /^[0-9]{16}-[A-Za-z0-9][A-Za-z0-9._-]*\.json$/.test(name)), `inbox names sort in send order: ${inboxNames.join(", ")}`);
  assert.deepEqual([...inboxNames].sort(), inboxNames);
});

test("the harness is uploaded when the Sandbox image has none, and a transient access failure is retried", async () => {
  const stores = new FakeStores(), sandbox = new FakeSandbox();
  stores.state.sandboxState = "ready"; stores.state.nodeId = nodeId; sandbox.imageHasAgent = false; sandbox.failExec = 1;
  await build(stores, sandbox, stubRoute).execute(item(), new AbortController().signal);
  assert.equal(sandbox.uploads.get("/workspace/artifacts/blazn-agent-bin")!.toString(), "binary");
  assert.ok(sandbox.commands.some((command) => command[0] === "/bin/sh" && command[2]!.includes("chmod 0700")));
  assert.ok(sandbox.commands.some((command) => command[0] === "/workspace/artifacts/blazn-agent-bin" && command[1] === "start"));
  assert.equal(stores.finalized?.outcome, "succeeded");
});

test("a model failure is reported to the user and the Run still finalizes; a missing route fails the Run", async () => {
  const failing = new FakeStores(), sandbox = new FakeSandbox();
  failing.state.sandboxState = "ready"; failing.state.nodeId = nodeId;
  const routes = parseModelRoutes(JSON.stringify({ routes: [{ id: routeId, version: 1, kind: "openai-chat", model: "m", baseUrl: "http://127.0.0.1:9/v1", timeoutSeconds: 5 }] }));
  const router = new ModelRouter(routes, (async () => new Response("{}", { status: 401 })) as typeof fetch);
  const executor = build(failing, sandbox, routes);
  (executor as unknown as { models: ModelRouter }).models = router;
  await executor.execute(item(), new AbortController().signal);
  assert.deepEqual(failing.replies, [{ sequence: 4, parent: messageId, content: "The agent could not finish this message (model_credential_rejected)." }]);
  assert.deepEqual(failing.delivered, [messageId]);
  assert.equal(failing.finalized?.outcome, "succeeded");

  const unrouted = new FakeStores();
  unrouted.state.sandboxState = "ready"; unrouted.state.nodeId = nodeId;
  await build(unrouted, new FakeSandbox(), []).execute(item(), new AbortController().signal);
  assert.deepEqual(unrouted.finalized, { outcome: "failed", errorCode: "model_route_unavailable", artifactIds: [], steps: 0 });
});

test("a Sandbox that never becomes ready is retried, and a lost lease abandons the Run without finalizing", async () => {
  const stuck = new FakeStores();
  await assert.rejects(() => build(stuck, new FakeSandbox(), stubRoute, { sandboxReadySeconds: 10 }).execute(item(), new AbortController().signal), (error: unknown) => error instanceof RunRetryable && error.code === "sandbox_not_ready");
  assert.equal(stuck.finalized, undefined);

  const unobserved = new FakeStores(); unobserved.state.sandboxState = "ready";
  await assert.rejects(() => build(unobserved, new FakeSandbox(), stubRoute).execute(item(), new AbortController().signal), (error: unknown) => error instanceof RunRetryable && error.code === "sandbox_node_unobserved");
  assert.equal(unobserved.bound, 0);

  const failed = new FakeStores(); failed.state.sandboxState = "failed";
  await assert.rejects(() => build(failed, new FakeSandbox(), stubRoute).execute(item(), new AbortController().signal), (error: unknown) => error instanceof RunRetryable && error.code === "sandbox_unavailable");

  const fenced = new FakeStores(), sandbox = new FakeSandbox();
  fenced.state.sandboxState = "ready"; fenced.state.nodeId = nodeId;
  const record = fenced.recordEvent.bind(fenced);
  fenced.recordEvent = async (lease, sequence, type, payload) => { if (sequence >= 3) fenced.leaseHeld = false; return record(lease, sequence, type, payload); };
  await assert.rejects(() => build(fenced, sandbox, stubRoute).execute(item(), new AbortController().signal), RunLeaseLost);
  assert.equal(fenced.finalized, undefined);
});

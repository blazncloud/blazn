import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { AgentRunControllerService } from "./agent-run-controller-service.js";
import type { AgentRunControllerStore, AgentRunWorkItem } from "./agent-run-controller-store.js";
import type { AgentRunExecution, AgentRunExecutionStore, AgentRunLease, AgentRunMessageClaim } from "./agent-run-execution-store.js";
import { AgentRunExecutor } from "./agent-run-executor.js";
import { ModelRouter, parseModelRoutes } from "./agent-run-model.js";
import type { SandboxAccess, SandboxExecResult } from "./agent-run-sandbox-access.js";

/**
 * Runs the executor against the real blazn-agent binary inside a container
 * that is set up like a Sandbox (read-only root, uid 65532, no network).
 *
 *   BLAZN_HARNESS_IT_CONTAINER  name of a running container with /workspace/artifacts
 *                               and a git checkout at /workspace/src/blazn
 *   BLAZN_HARNESS_IT_BINARY     path to blazn-agent for the container's architecture
 */
const container = process.env.BLAZN_HARNESS_IT_CONTAINER, binaryPath = process.env.BLAZN_HARNESS_IT_BINARY;
const routeId = "60000000-0000-4000-8000-000000000001", nodeId = "70000000-0000-4000-8000-000000000001", sandboxId = "80000000-0000-4000-8000-000000000001";

function docker(args: string[], input?: Buffer, timeoutMs = 60_000): Promise<{ code: number; stdout: Buffer; stderr: Buffer }> {
  return new Promise((resolve, reject) => {
    const child = execFile("docker", args, { encoding: "buffer", maxBuffer: 64 * 1024 * 1024, timeout: timeoutMs }, (error, stdout, stderr) => {
      if (error && typeof (error as { code?: unknown }).code !== "number") return reject(error);
      resolve({ code: error ? (error as unknown as { code: number }).code : 0, stdout, stderr });
    });
    child.stdin!.end(input ?? Buffer.alloc(0));
  });
}

/** Sandbox access with the access endpoint's limits: bounded argv, no stdin, no environment, 60 s. */
class ContainerSandbox implements SandboxAccess {
  commands: string[][] = [];
  async exec(_lease: AgentRunLease, command: string[]): Promise<SandboxExecResult> {
    assert.ok(command.length <= 32 && command.every((part) => Buffer.byteLength(part) <= 1024), "exec argv stays inside the Sandbox limits");
    this.commands.push(command);
    const result = await docker(["exec", container!, ...command]);
    return { exitCode: result.code, stdout: result.stdout, stderr: result.stderr, truncated: false };
  }
  async upload(_lease: AgentRunLease, path: string, content: Buffer) {
    assert.ok(path.startsWith("/workspace/artifacts/") || path.startsWith("/workspace/src/"), `upload path ${path}`);
    const result = await docker(["exec", "-i", container!, "/bin/sh", "-c", 'cat > "$1"', "sh", path], content);
    if (result.code !== 0) throw new Error(`upload failed: ${result.stderr.toString()}`);
  }
  async download(_lease: AgentRunLease, path: string) {
    const result = await docker(["exec", container!, "/bin/cat", path]);
    if (result.code !== 0) throw new Error(`download failed: ${result.stderr.toString()}`);
    return result.stdout;
  }
}

class MemoryStores implements AgentRunExecutionStore, AgentRunControllerStore {
  state: AgentRunExecution = { sandboxId, sandboxState: "ready", sandboxExpiresAt: new Date(Date.now() + 3600_000).toISOString(), architecture: "amd64", nodeId,
    outboxSequence: 0, runStatus: "queued", runVersion: 1, instructions: "You are a careful coding agent.", purpose: "integration", repositoryDestination: "/workspace/src/blazn" };
  queue: AgentRunMessageClaim[] = [];
  claimed: AgentRunMessageClaim | undefined;
  replies: { parent?: string; content: string }[] = [];
  events: (string | null)[] = [];
  delivered: string[] = [];
  artifacts = new Map<string, Buffer>();
  finalized?: { outcome: string; errorCode?: string };
  async execution() { return { ...this.state }; }
  async issueGrant() { return true; }
  async claimMessage() { if (this.claimed) return { ...this.claimed, resumed: true }; this.claimed = this.queue.shift(); return this.claimed; }
  async deliverMessage(_lease: AgentRunLease, id: string) { this.delivered.push(id); this.claimed = undefined; return true; }
  async recordReply(_lease: AgentRunLease, sequence: number, parent: string | undefined, content: string) { this.replies.push(parent === undefined ? { content } : { parent, content }); return `00000000-0000-4000-8000-${String(sequence).padStart(12, "0")}`; }
  async recordEvent(_lease: AgentRunLease, sequence: number, type: string | null) { if (sequence > this.state.outboxSequence) { this.events.push(type); this.state.outboxSequence = sequence; } return true; }
  async recordArtifact(_lease: AgentRunLease, name: "patch" | "summary", content: Buffer) { this.artifacts.set(name, content); return name === "patch" ? "f0000000-0000-4000-8000-000000000001" : "f0000000-0000-4000-8000-000000000002"; }
  async releaseSandboxes() { return 0; }
  async enqueue() { return true; }
  async claim() { return undefined; }
  async renew() { return new Date().toISOString(); }
  async bindSandbox() { this.state.runStatus = "running"; this.state.runVersion++; return true; }
  async retry() { return "retry_scheduled" as const; }
  async finalize(_run: string, _worker: string, _token: string, _version: number, outcome: "succeeded" | "failed", errorCode: string | undefined) {
    this.finalized = errorCode === undefined ? { outcome } : { outcome, errorCode }; this.state.runStatus = outcome; return true;
  }
}

function item(): AgentRunWorkItem {
  const digest = `sha256:${"a".repeat(64)}`;
  return { runId: "50000000-0000-4000-8000-000000000001", workspaceId: "10000000-0000-4000-8000-000000000001", projectId: "20000000-0000-4000-8000-000000000001", runVersion: 1, leaseToken: "30000000-0000-4000-8000-000000000001",
    leaseExpiresAt: "2026-10-01T00:00:00Z", attempt: 1, requestedBy: "40000000-0000-4000-8000-000000000001", planDigest: digest, agentVersionId: "a0000000-0000-4000-8000-000000000001",
    agentVersionDigest: digest, agentVersion: {}, harnessDefinitionId: "b0000000-0000-4000-8000-000000000001", harnessVersionId: "c0000000-0000-4000-8000-000000000001", harnessVersionDigest: digest,
    harnessVersion: {}, harnessProfileId: "d0000000-0000-4000-8000-000000000001", harnessProfileDigest: digest, harnessProfile: {}, templateVersionId: "e0000000-0000-4000-8000-000000000001",
    templateDigest: digest, modelRouteId: routeId, modelRouteVersion: 1, modelProtocol: "openai-chat" };
}

test("the executor drives the real harness through two turns and collects its patch", { skip: !container || !binaryPath, timeout: 240_000 }, async () => {
  const stores = new MemoryStores(), sandbox = new ContainerSandbox(), binary = await readFile(binaryPath!);
  const first = "90000000-0000-4000-8000-000000000001", second = "90000000-0000-4000-8000-000000000003";
  stores.queue.push({ messageId: first, claimId: "90000000-0000-4000-8000-000000000002", ordinal: 1, kind: "prompt", content: "first message", resumed: false });
  // The follow-up arrives only after the first reply, as it would from a user.
  const record = stores.recordReply.bind(stores);
  stores.recordReply = async (lease, sequence, parent, content) => {
    if (parent === first) stores.queue.push({ messageId: second, claimId: "90000000-0000-4000-8000-000000000004", ordinal: 2, kind: "followup", content: "second message", resumed: false });
    return record(lease, sequence, parent, content);
  };
  const routes = parseModelRoutes(JSON.stringify({ routes: [{ id: routeId, version: 1, kind: "stub", model: "stub-model" }] }));
  const executor = new AgentRunExecutor(new AgentRunControllerService(stores), stores, sandbox, new ModelRouter(routes), {
    workerId: "worker-it", agentDirectory: "/workspace/artifacts/blazn-agent", imageAgentPath: "/opt/blazn/agent", uploadedAgentPath: "/workspace/artifacts/blazn-agent-bin",
    harnessBinary: async () => binary, sandboxReadySeconds: 30, nodeObservationSeconds: 10, idleSeconds: 6, waitSeconds: 3, messageLeaseSeconds: 300, expiryMarginSeconds: 60,
    maxStepsPerTurn: 10, commandTimeoutSeconds: 30,
  });
  await executor.execute(item(), new AbortController().signal);

  assert.deepEqual(stores.finalized, { outcome: "succeeded" });
  assert.deepEqual(stores.delivered, [first, second]);
  assert.deepEqual(stores.replies, [
    { parent: first, content: 'Stub model: the sandbox command printed "blazn stub model saw: first message".' },
    { parent: second, content: 'Stub model: the sandbox command printed "blazn stub model saw: second message".' },
  ]);
  const patch = stores.artifacts.get("patch")!.toString();
  assert.match(patch, /BLAZN_AGENT_PROOF\.md/);
  assert.match(patch, /\+blazn stub model saw: second message/);
  assert.ok(stores.artifacts.get("summary")!.length > 0);
  assert.ok(sandbox.commands.some((command) => command[0] === "/workspace/artifacts/blazn-agent-bin" && command[1] === "start"), "the uploaded harness was started");
  assert.ok(stores.events.includes("harness-finalized"));
});

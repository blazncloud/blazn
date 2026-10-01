import type { AgentRunControllerService } from "./agent-run-controller-service.js";
import type { AgentRunWorkItem } from "./agent-run-controller-store.js";
import type { AgentRunExecution, AgentRunExecutionStore, AgentRunLease, AgentRunMessageClaim } from "./agent-run-execution-store.js";
import type { ModelRoute, ModelRouter } from "./agent-run-model.js";
import { SandboxAccessError, type SandboxAccess, type SandboxExecResult } from "./agent-run-sandbox-access.js";

/** The controller no longer holds the Run: another worker has it, or it ended. */
export class RunLeaseLost extends Error { constructor() { super("Agent Run lease was lost"); } }
/** Worth another attempt by a later claim. */
export class RunRetryable extends Error { constructor(readonly code: string) { super(code); } }
/** The Run cannot succeed; it is finalized as failed with this code. */
export class RunFatal extends Error { constructor(readonly code: string) { super(code); } }

export interface AgentRunExecutorOptions {
  workerId: string;
  /** State directory of the harness inside the Sandbox. */
  agentDirectory: string;
  /** Harness path baked into the Sandbox image, tried first. */
  imageAgentPath: string;
  /** Where the controller uploads the harness when the image has none. */
  uploadedAgentPath: string;
  /** Returns the harness executable for an architecture, when the controller ships one. */
  harnessBinary: (architecture: "amd64" | "arm64") => Promise<Buffer | undefined>;
  sandboxReadySeconds: number;
  idleSeconds: number;
  waitSeconds: number;
  messageLeaseSeconds: number;
  expiryMarginSeconds: number;
  maxStepsPerTurn: number;
  commandTimeoutSeconds: number;
  now?: () => number;
  sleep?: (milliseconds: number, signal: AbortSignal) => Promise<void>;
}

interface HarnessEvent { seq: number; type: string; data: Record<string, unknown> }
interface WaitResult { events: HarnessEvent[]; alive: boolean }
interface Finalized { artifacts: { name: string; path: string }[]; steps: number; warnings: string[] }

const SCHEMA = "blazn.dev/sandbox-agent/v1alpha1";
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const deadSandboxStates = ["failed", "stopping", "stopped", "deleting", "deleted"];

/**
 * Runs one claimed Agent Run: waits for its Sandbox, starts the harness in
 * it, relays user messages in and replies out, answers the harness's model
 * requests, and finalizes the Run with its patch and summary.
 */
export class AgentRunExecutor {
  private readonly now: () => number;
  private readonly sleep: (milliseconds: number, signal: AbortSignal) => Promise<void>;
  constructor(private readonly controller: AgentRunControllerService, private readonly store: AgentRunExecutionStore,
    private readonly access: SandboxAccess, private readonly models: ModelRouter, private readonly options: AgentRunExecutorOptions) {
    this.now = options.now ?? Date.now;
    this.sleep = options.sleep ?? delay;
  }

  async execute(item: AgentRunWorkItem, signal: AbortSignal): Promise<void> {
    const lease: AgentRunLease = { runId: item.runId, workerId: this.options.workerId, leaseToken: item.leaseToken };
    let execution = await this.current(lease);
    const deadline = this.now() + this.options.sandboxReadySeconds * 1000;
    while (!((execution.sandboxState === "ready" || execution.sandboxState === "running") && execution.nodeId)) {
      if (deadSandboxStates.includes(execution.sandboxState)) throw new RunRetryable("sandbox_unavailable");
      if (this.now() > deadline) throw new RunRetryable("sandbox_not_ready");
      await this.sleep(2000, signal);
      execution = await this.current(lease);
    }
    if (execution.runStatus === "queued") {
      const bound = await this.controller.bindSandbox(item.runId, lease.workerId, lease.leaseToken, execution.runVersion, execution.nodeId!, execution.sandboxId);
      if (!bound) throw new RunRetryable("sandbox_bind_rejected");
    }
    // From here the Run is running in its Sandbox: a failure ends the Run.
    let steps = 0;
    try {
      const result = await this.converse(lease, item, execution, signal, (count) => { steps = count; });
      const artifactIds: string[] = [];
      for (const name of ["patch", "summary"] as const) {
        const artifact = result.artifacts.find((entry) => entry.name === name);
        if (!artifact) throw new RunFatal("harness_output_missing");
        const content = await this.retrying(lease, () => this.access.download(lease, artifact.path));
        const id = await this.store.recordArtifact(lease, name, content);
        if (!id) throw new RunFatal("artifact_record_rejected");
        artifactIds.push(id);
      }
      // A Run may only succeed with every accepted message delivered. Messages
      // that arrived after the harness finalized are answered here.
      for (let pending = await this.store.claimMessage(lease, 30); pending; pending = await this.store.claimMessage(lease, 30)) {
        await this.store.recordReply(lease, 2_000_000_000 + pending.ordinal, pending.messageId, "This run had already finished, so the message was not processed. Start a new run to continue.");
        if (!await this.store.deliverMessage(lease, pending.messageId, pending.claimId)) throw new RunLeaseLost();
      }
      const final = await this.current(lease);
      const completed = await this.controller.finalize(item.runId, lease.workerId, lease.leaseToken, final.runVersion, "succeeded", undefined, artifactIds, result.steps, result.warnings.slice(0, 100));
      if (!completed) throw new RunLeaseLost();
    } catch (error) {
      if (error instanceof RunLeaseLost || signal.aborted) throw error;
      const code = error instanceof RunFatal || error instanceof RunRetryable ? error.code : error instanceof SandboxAccessError ? "sandbox_access_failed" : "controller_execution_failed";
      const final = await this.store.execution(lease);
      if (!final) throw new RunLeaseLost();
      const failed = await this.controller.finalize(item.runId, lease.workerId, lease.leaseToken, final.runVersion, "failed", code, [], steps, []);
      if (!failed) throw new RunLeaseLost();
    }
  }

  private async current(lease: AgentRunLease): Promise<AgentRunExecution> {
    const execution = await this.store.execution(lease);
    if (!execution) throw new RunLeaseLost();
    return execution;
  }

  private async converse(lease: AgentRunLease, item: AgentRunWorkItem, execution: AgentRunExecution, signal: AbortSignal, onSteps: (steps: number) => void): Promise<Finalized> {
    const route = this.models.route(item.modelRouteId, item.modelRouteVersion);
    if (!route) throw new RunFatal("model_route_unavailable");
    const agent = await this.bootstrap(lease, item, execution, route);
    const directory = this.options.agentDirectory;
    let sequence = execution.outboxSequence, steps = 0, restarts = 0, inboxNumber = 0;
    let currentMessage: AgentRunMessageClaim | undefined, lastClaimRenewal = 0, idleSince = this.now();
    let finalizing = false, finalizeDeadline = 0, finalized: Finalized | undefined;
    const inbox = async (id: string, value: Record<string, unknown>) => {
      // Fixed-width names sort in send order: milliseconds, then a counter for ties.
      const name = `${String(this.now()).padStart(13, "0")}${String(inboxNumber++ % 1000).padStart(3, "0")}-${id}.json`;
      await this.retrying(lease, () => this.access.upload(lease, `${directory}/inbox/${name}`, Buffer.from(JSON.stringify(value))));
    };
    const record = async (seq: number, type: string | null, payload: Record<string, unknown> = {}) => {
      if (!await this.store.recordEvent(lease, seq, type, payload)) throw new RunLeaseLost();
    };

    while (!finalized) {
      if (signal.aborted) throw signal.reason instanceof Error ? signal.reason : new Error("Agent Run controller is stopping");
      if (!currentMessage && !finalizing) {
        const claim = await this.store.claimMessage(lease, this.options.messageLeaseSeconds);
        if (claim) {
          await inbox(claim.messageId, { type: "message", id: claim.messageId, kind: claim.kind, content: claim.content });
          currentMessage = claim; lastClaimRenewal = this.now();
        } else {
          const live = await this.current(lease);
          if (deadSandboxStates.includes(live.sandboxState)) throw new RunFatal("sandbox_unavailable");
          const expiring = Date.parse(live.sandboxExpiresAt) - this.now() < this.options.expiryMarginSeconds * 1000;
          if (expiring || this.now() - idleSince > this.options.idleSeconds * 1000) {
            await inbox("finalize", { type: "finalize" });
            finalizing = true; finalizeDeadline = this.now() + 180_000;
          } else if (sequence > 0) {
            await this.sleep(1000, signal); // idle: nothing to read from the harness until a message arrives
            continue;
          }
        }
      } else if (currentMessage && this.now() - lastClaimRenewal > this.options.messageLeaseSeconds * 1000 / 3) {
        if (!await this.store.claimMessage(lease, this.options.messageLeaseSeconds)) throw new RunLeaseLost();
        lastClaimRenewal = this.now();
      }
      if (finalizing && this.now() > finalizeDeadline) throw new RunFatal("harness_finalize_timeout");

      const waited = await this.wait(lease, agent, sequence, signal);
      for (const event of waited.events) {
        switch (event.type) {
          case "model.request": {
            const requestId = text(event.data.requestId, 64);
            if (!requestId || event.data.request === undefined) { await record(event.seq, null); break; }
            const outcome = await this.models.complete(route, event.data.request, signal);
            steps++; onSteps(steps);
            await inbox(requestId, outcome.ok ? { type: "model.response", requestId, response: outcome.response } : { type: "model.response", requestId, error: outcome.error });
            const payload: Record<string, unknown> = { requestId, routeId: route.id, routeVersion: route.version, model: route.model, ok: outcome.ok, durationMs: outcome.durationMs };
            if (outcome.ok) { if (outcome.usage.promptTokens !== undefined) payload.inputUnits = outcome.usage.promptTokens; if (outcome.usage.completionTokens !== undefined) payload.outputUnits = outcome.usage.completionTokens; }
            else payload.errorCode = outcome.error.code;
            await record(event.seq, "model-request", payload);
            break;
          }
          case "assistant.message": {
            const content = clip(typeof event.data.content === "string" && event.data.content.trim() ? event.data.content : "(the agent returned no text)");
            if (await this.store.recordReply(lease, event.seq, uuid(event.data.messageId), content) === undefined) throw new RunLeaseLost();
            await record(event.seq, null);
            break;
          }
          case "turn.completed": {
            const messageId = uuid(event.data.messageId), status = text(event.data.status, 16), errorCode = code(event.data.errorCode);
            if (status !== "completed") {
              if (await this.store.recordReply(lease, event.seq, messageId, `The agent could not finish this message (${errorCode ?? status ?? "failed"}).`) === undefined) throw new RunLeaseLost();
            }
            if (currentMessage && messageId === currentMessage.messageId) {
              if (!await this.store.deliverMessage(lease, currentMessage.messageId, currentMessage.claimId)) throw new RunLeaseLost();
              currentMessage = undefined; idleSince = this.now();
            }
            const payload: Record<string, unknown> = { status };
            if (messageId) payload.messageId = messageId;
            if (errorCode) payload.errorCode = errorCode;
            if (Number.isSafeInteger(event.data.steps)) payload.steps = event.data.steps;
            await record(event.seq, "turn-completed", payload);
            break;
          }
          case "message.accepted": await record(event.seq, "turn-started", uuid(event.data.messageId) ? { messageId: uuid(event.data.messageId) } : {}); break;
          case "tool.started": await record(event.seq, "tool-started", { callId: text(event.data.callId, 128), name: text(event.data.name, 64), summary: text(event.data.summary, 200) }); break;
          case "tool.finished": await record(event.seq, "tool-finished", { callId: text(event.data.callId, 128), name: text(event.data.name, 64), ok: event.data.ok === true,
            exitCode: Number.isSafeInteger(event.data.exitCode) ? event.data.exitCode : null, truncated: event.data.truncated === true }); break;
          case "started": await record(event.seq, "harness-started", { version: text(event.data.version, 32) }); break;
          case "finalized": {
            const artifacts = Array.isArray(event.data.artifacts) ? event.data.artifacts.flatMap((entry) => {
              const value = entry as Record<string, unknown>, name = text(value.name, 32), path = text(value.path, 512);
              return name && path.startsWith(`${directory}/out/`) ? [{ name, path }] : [];
            }) : [];
            const warnings = Array.isArray(event.data.warnings) ? event.data.warnings.flatMap((entry) => typeof entry === "string" && entry ? [entry.slice(0, 512)] : []) : [];
            finalized = { artifacts, steps: Number.isSafeInteger(event.data.steps) ? event.data.steps as number : steps, warnings };
            await record(event.seq, "harness-finalized", { artifacts: artifacts.length });
            break;
          }
          default: await record(event.seq, null);
        }
        sequence = event.seq;
      }
      if (!waited.alive && waited.events.length === 0 && !finalized) {
        if (++restarts > 3) throw new RunFatal("harness_unavailable");
        await this.command(lease, [agent, "start", "--dir", directory], "harness_start_failed");
      }
    }
    return finalized;
  }

  /** Makes sure the harness is present, configured and running in the Sandbox. Safe to repeat. */
  private async bootstrap(lease: AgentRunLease, item: AgentRunWorkItem, execution: AgentRunExecution, route: ModelRoute): Promise<string> {
    const directory = this.options.agentDirectory;
    let agent = this.options.imageAgentPath;
    if (!await this.succeeds(lease, [agent, "version"])) {
      agent = this.options.uploadedAgentPath;
      if (!await this.succeeds(lease, [agent, "version"])) {
        const binary = await this.options.harnessBinary(execution.architecture);
        if (!binary) throw new RunFatal("harness_unavailable");
        await this.retrying(lease, () => this.access.upload(lease, agent, binary));
        await this.command(lease, ["/bin/sh", "-c", `chmod 0700 '${agent}'`], "harness_install_failed");
        if (!await this.succeeds(lease, [agent, "version"])) throw new RunFatal("harness_install_failed");
      }
    }
    await this.command(lease, [agent, "init", "--dir", directory], "harness_init_failed");
    const workingDirectory = execution.repositoryDestination ?? "/workspace/artifacts";
    const config: Record<string, unknown> = {
      schemaVersion: SCHEMA, runId: item.runId, instructions: execution.instructions || "You are a helpful coding agent.", workingDirectory,
      model: route.model, maxStepsPerTurn: this.options.maxStepsPerTurn, commandTimeoutSeconds: this.options.commandTimeoutSeconds,
      modelTimeoutSeconds: Math.min(1800, route.timeoutSeconds * 2 + 60),
    };
    if (execution.purpose) config.purpose = execution.purpose.slice(0, 512);
    if (execution.repositoryDestination) config.repositoryDirectory = execution.repositoryDestination;
    if (route.maxOutputTokens !== undefined) config.maxOutputTokens = route.maxOutputTokens;
    await this.retrying(lease, () => this.access.upload(lease, `${directory}/config.json`, Buffer.from(JSON.stringify(config))));
    await this.command(lease, [agent, "start", "--dir", directory], "harness_start_failed");
    return agent;
  }

  private async wait(lease: AgentRunLease, agent: string, after: number, signal: AbortSignal): Promise<WaitResult> {
    if (signal.aborted) throw signal.reason instanceof Error ? signal.reason : new Error("Agent Run controller is stopping");
    const result = await this.retrying(lease, () => this.access.exec(lease, [agent, "wait", "--dir", this.options.agentDirectory, "--after", String(after), "--timeout", String(this.options.waitSeconds)],
      (this.options.waitSeconds + 25) * 1000));
    if (result.exitCode !== 0) throw new RunFatal("harness_wait_failed");
    const events: HarnessEvent[] = [];
    let alive = true;
    for (const line of result.stdout.toString("utf8").split("\n")) {
      if (!line) continue;
      let parsed: { seq?: unknown; type?: unknown; data?: unknown; alive?: unknown };
      try { parsed = JSON.parse(line); } catch { throw new RunFatal("harness_output_invalid"); }
      if (parsed.type === "wait.status") { alive = parsed.alive === true; continue; }
      if (!Number.isSafeInteger(parsed.seq) || (parsed.seq as number) <= after || typeof parsed.type !== "string") throw new RunFatal("harness_output_invalid");
      events.push({ seq: parsed.seq as number, type: parsed.type, data: parsed.data && typeof parsed.data === "object" && !Array.isArray(parsed.data) ? parsed.data as Record<string, unknown> : {} });
    }
    return { events, alive };
  }

  private async succeeds(lease: AgentRunLease, command: string[]): Promise<boolean> {
    try { return (await this.retrying(lease, () => this.access.exec(lease, command, 30_000))).exitCode === 0; }
    catch (error) { if (error instanceof SandboxAccessError && error.code !== "grant_denied") return false; throw error; }
  }

  private async command(lease: AgentRunLease, command: string[], failure: string): Promise<SandboxExecResult> {
    const result = await this.retrying(lease, () => this.access.exec(lease, command, 30_000));
    if (result.exitCode !== 0) throw new RunFatal(failure);
    return result;
  }

  /** Retries a transient Sandbox access failure; a refused grant means the lease or Sandbox is gone. */
  private async retrying<T>(lease: AgentRunLease, operation: () => Promise<T>): Promise<T> {
    for (let attempt = 1; ; attempt++) {
      try { return await operation(); } catch (error) {
        if (!(error instanceof SandboxAccessError)) throw error;
        if (error.code === "grant_denied") {
          const execution = await this.current(lease);
          throw new RunFatal(deadSandboxStates.includes(execution.sandboxState) ? "sandbox_unavailable" : "sandbox_access_denied");
        }
        if (attempt >= 3) throw error;
        await delay(attempt * 1500, AbortSignal.timeout(60_000));
      }
    }
  }
}

function text(value: unknown, max: number): string { return typeof value === "string" ? value.slice(0, max) : ""; }
function uuid(value: unknown): string | undefined { return typeof value === "string" && UUID.test(value) ? value : undefined; }
function code(value: unknown): string | undefined { return typeof value === "string" && /^[a-z][a-z0-9_]{0,62}$/.test(value) ? value : undefined; }
/** Run messages hold at most 16384 characters. */
function clip(content: string): string {
  const characters = Array.from(content);
  return characters.length <= 16384 ? content : `${characters.slice(0, 16300).join("")}\n\n[reply truncated]`;
}
function delay(milliseconds: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) { reject(signal.reason); return; }
    const abort = () => { clearTimeout(timer); reject(signal.reason); };
    const timer = setTimeout(() => { signal.removeEventListener("abort", abort); resolve(); }, milliseconds);
    signal.addEventListener("abort", abort, { once: true });
  });
}

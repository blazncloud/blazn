import { AgentRunControllerService } from "./agent-run-controller-service.js";
import type { AgentRunControllerStore, AgentRunWorkItem } from "./agent-run-controller-store.js";
import type { AgentRunExecutionStore } from "./agent-run-execution-store.js";
import { RunLeaseLost, RunRetryable, type AgentRunExecutor } from "./agent-run-executor.js";

export interface AgentRunControllerRuntimeOptions {
  workerId: string;
  leaseSeconds: number;
  retryDelaySeconds: number;
  pollMilliseconds: number;
  maxConcurrentRuns: number;
}
export type AgentRunRuntimeEvent = { type: "claimed" | "completed" | "retry" | "lease-lost" | "failed" | "released"; runId?: string; attempt?: number; detail?: string };

/**
 * Claims queued Agent Runs and executes each one concurrently under a renewed
 * lease. A Run whose lease is lost is abandoned at once; another claim resumes
 * it from the harness's recorded position.
 */
export class AgentRunControllerRuntime {
  private readonly service: AgentRunControllerService;
  private readonly active = new Map<string, Promise<void>>();
  constructor(store: AgentRunControllerStore, private readonly executions: AgentRunExecutionStore, private readonly executor: AgentRunExecutor,
    private readonly options: AgentRunControllerRuntimeOptions, private readonly event: (event: AgentRunRuntimeEvent) => void = () => {}) {
    this.service = new AgentRunControllerService(store);
  }

  get controller(): AgentRunControllerService { return this.service; }

  async run(signal: AbortSignal): Promise<void> {
    let lastRelease = 0;
    while (!signal.aborted) {
      if (Date.now() - lastRelease > 5000) {
        lastRelease = Date.now();
        const released = await this.executions.releaseSandboxes(10).catch(() => 0);
        if (released > 0) this.event({ type: "released", detail: String(released) });
      }
      let claimed = false;
      if (this.active.size < this.options.maxConcurrentRuns) {
        const item = await this.service.claim(this.options.workerId, this.options.leaseSeconds);
        if (item) {
          claimed = true;
          this.event({ type: "claimed", runId: item.runId, attempt: item.attempt });
          const task = this.supervise(item, signal).finally(() => this.active.delete(item.runId));
          this.active.set(item.runId, task);
        }
      }
      if (!claimed) await delay(this.options.pollMilliseconds, signal).catch(() => undefined);
    }
    await Promise.allSettled(this.active.values());
  }

  private async supervise(item: AgentRunWorkItem, parent: AbortSignal): Promise<void> {
    const controller = new AbortController(), abort = () => controller.abort(parent.reason);
    parent.addEventListener("abort", abort, { once: true });
    if (parent.aborted) abort();
    let renewing = false;
    const interval = setInterval(() => {
      if (renewing || controller.signal.aborted) return;
      renewing = true;
      this.service.renew(item.runId, this.options.workerId, item.leaseToken, this.options.leaseSeconds)
        .then((until) => { if (!until) controller.abort(new RunLeaseLost()); })
        .catch(() => controller.abort(new RunLeaseLost()))
        .finally(() => { renewing = false; });
    }, Math.max(1000, Math.floor(this.options.leaseSeconds * 1000 / 3)));
    interval.unref();
    try {
      await this.executor.execute(item, controller.signal);
      this.event({ type: "completed", runId: item.runId, attempt: item.attempt });
    } catch (error) {
      if (error instanceof RunLeaseLost || controller.signal.reason instanceof RunLeaseLost) { this.event({ type: "lease-lost", runId: item.runId, attempt: item.attempt }); return; }
      if (parent.aborted) return; // shutting down: the lease expires and another claim resumes the Run
      const code = error instanceof RunRetryable ? error.code : "controller_execution_failed";
      const outcome = await this.service.retry(item.runId, this.options.workerId, item.leaseToken, this.options.retryDelaySeconds, code).catch(() => "fenced" as const);
      this.event({ type: outcome === "retry_scheduled" ? "retry" : outcome === "fenced" ? "lease-lost" : "failed", runId: item.runId, attempt: item.attempt, detail: code });
    } finally {
      clearInterval(interval);
      parent.removeEventListener("abort", abort);
    }
  }
}

function delay(milliseconds: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) { reject(signal.reason); return; }
    const abort = () => { clearTimeout(timer); reject(signal.reason); };
    const timer = setTimeout(() => { signal.removeEventListener("abort", abort); resolve(); }, milliseconds);
    signal.addEventListener("abort", abort, { once: true });
  });
}

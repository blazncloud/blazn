import { readFile } from "node:fs/promises";
import { AgentRunControllerRuntime } from "./agent-run-controller-runtime.js";
import { PgAgentRunControllerStore } from "./agent-run-controller-store.js";
import { PgAgentRunExecutionStore } from "./agent-run-execution-store.js";
import { AgentRunExecutor, type AgentRunExecutorOptions } from "./agent-run-executor.js";
import { ModelRouter, parseModelRoutes } from "./agent-run-model.js";
import { GrantSandboxAccess } from "./agent-run-sandbox-access.js";
import { AgentRunControllerService } from "./agent-run-controller-service.js";
import { createDatabase } from "./db.js";

/**
 * The Agent Run controller: a separate least-privilege process that executes
 * queued Agent Runs. It connects as blazn_agent_run_controller, which can only
 * call the lease-fenced controller functions.
 *
 *   BLAZN_AGENT_RUN_DATABASE_URL_FILE  connection string for blazn_agent_run_controller
 *   BLAZN_SANDBOX_ACCESS_URL           the Sandbox controller's access endpoint
 *   BLAZN_MODEL_ROUTES_FILE            JSON model routes (see docs/agent-run-controller.md)
 *   BLAZN_AGENT_HARNESS_DIR            directory holding blazn-agent-linux-<arch>
 */
export async function startAgentRunController(signal: AbortSignal): Promise<void> {
  const config = await loadAgentRunControllerConfig();
  const database = createDatabase(config.databaseUrl);
  const onError = (error: Error) => process.stderr.write(`Agent Run controller database pool error code=${databaseCode(error)}\n`);
  database.on("error", onError);
  try {
    await database.query("SELECT 1");
    const controllerStore = new PgAgentRunControllerStore(database), executions = new PgAgentRunExecutionStore(database);
    const executor = new AgentRunExecutor(new AgentRunControllerService(controllerStore), executions, new GrantSandboxAccess(executions, config.sandboxAccessUrl), new ModelRouter(config.routes), config.executor);
    const runtime = new AgentRunControllerRuntime(controllerStore, executions, executor, config.runtime, (event) => process.stdout.write(`${JSON.stringify({ component: "agent-run-controller", ...event })}\n`));
    process.stdout.write(`${JSON.stringify({ component: "agent-run-controller", type: "started", workerId: config.runtime.workerId, routes: config.routes.length })}\n`);
    await runtime.run(signal);
  } finally {
    database.off("error", onError);
    await database.end();
  }
}

export async function loadAgentRunControllerConfig(env: NodeJS.ProcessEnv = process.env) {
  const required = (name: string) => { const value = env[name]; if (!value) throw new Error(`${name} is required`); return value; };
  const integer = (name: string, fallback: number, min: number, max: number) => {
    const raw = env[name];
    if (raw === undefined) return fallback;
    if (!/^[0-9]+$/.test(raw)) throw new Error(`${name} is invalid`);
    const value = Number(raw);
    if (!Number.isSafeInteger(value) || value < min || value > max) throw new Error(`${name} must be ${min} through ${max}`);
    return value;
  };
  const databaseUrl = (await readFile(required("BLAZN_AGENT_RUN_DATABASE_URL_FILE"), "utf8")).trim();
  if (!databaseUrl) throw new Error("Agent Run controller database URL is empty");
  const routes = parseModelRoutes(await readFile(required("BLAZN_MODEL_ROUTES_FILE"), "utf8"));
  const workerId = env.BLAZN_AGENT_RUN_WORKER_ID ?? "agent-run-controller-1";
  if (!/^[a-z0-9](?:[a-z0-9._-]{0,126}[a-z0-9])?$/.test(workerId)) throw new Error("BLAZN_AGENT_RUN_WORKER_ID is invalid");
  const harnessDirectory = env.BLAZN_AGENT_HARNESS_DIR ?? "/app/agent";
  if (!harnessDirectory.startsWith("/")) throw new Error("BLAZN_AGENT_HARNESS_DIR must be absolute");
  const binaries = new Map<string, Buffer | undefined>();
  const executor: AgentRunExecutorOptions = {
    workerId,
    agentDirectory: "/workspace/artifacts/blazn-agent",
    imageAgentPath: "/opt/blazn/agent",
    uploadedAgentPath: "/workspace/artifacts/blazn-agent-bin",
    harnessBinary: async (architecture) => {
      if (!binaries.has(architecture)) binaries.set(architecture, await readFile(`${harnessDirectory}/blazn-agent-linux-${architecture}`).catch(() => undefined));
      return binaries.get(architecture);
    },
    sandboxReadySeconds: integer("BLAZN_AGENT_RUN_SANDBOX_READY_SECONDS", 1000, 30, 3600),
    nodeObservationSeconds: integer("BLAZN_AGENT_RUN_NODE_OBSERVATION_SECONDS", 60, 5, 600),
    idleSeconds: integer("BLAZN_AGENT_RUN_IDLE_SECONDS", 300, 5, 7200),
    waitSeconds: integer("BLAZN_AGENT_RUN_WAIT_SECONDS", 20, 1, 30),
    messageLeaseSeconds: 300,
    expiryMarginSeconds: integer("BLAZN_AGENT_RUN_EXPIRY_MARGIN_SECONDS", 180, 30, 1800),
    maxStepsPerTurn: integer("BLAZN_AGENT_RUN_MAX_STEPS", 40, 1, 200),
    commandTimeoutSeconds: integer("BLAZN_AGENT_RUN_COMMAND_TIMEOUT_SECONDS", 300, 5, 1800),
  };
  return {
    databaseUrl, routes, executor, sandboxAccessUrl: required("BLAZN_SANDBOX_ACCESS_URL"),
    runtime: {
      workerId, leaseSeconds: integer("BLAZN_AGENT_RUN_LEASE_SECONDS", 90, 10, 300), retryDelaySeconds: integer("BLAZN_AGENT_RUN_RETRY_DELAY_SECONDS", 10, 0, 300),
      pollMilliseconds: integer("BLAZN_AGENT_RUN_POLL_MILLISECONDS", 1000, 100, 60000), maxConcurrentRuns: integer("BLAZN_AGENT_RUN_MAX_CONCURRENT", 4, 1, 32),
    },
  };
}

function databaseCode(error: Error) { return "code" in error && typeof error.code === "string" && /^[A-Z0-9]{5}$/.test(error.code) ? error.code : "unknown"; }

if (import.meta.url === `file://${process.argv[1]}`) {
  const controller = new AbortController();
  for (const event of ["SIGINT", "SIGTERM"] as const) process.once(event, () => controller.abort(new Error(event)));
  startAgentRunController(controller.signal).catch((error) => {
    if (!controller.signal.aborted) { process.stderr.write(`${error instanceof Error ? error.message : "Agent Run controller failed"}\n`); process.exitCode = 1; }
  });
}

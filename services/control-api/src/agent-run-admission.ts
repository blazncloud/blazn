import type { Database } from "./db.js";
import { RunHttpError, type Run } from "./run-types.js";
import type { SandboxService } from "./sandbox-service.js";
import { SandboxHttpError, type SandboxArchitecture, type SandboxPrincipal } from "./sandbox-types.js";

/** The Agent selection a caller may attach to a sandbox-proof Run. */
export interface AgentRunRequest {
  agentVersionId: string;
  harnessProfileId: string;
  architecture: SandboxArchitecture;
  expiresInSeconds?: number;
}

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

export function parseAgentRunRequest(value: unknown): AgentRunRequest {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw invalid("agent is invalid");
  const body = value as Record<string, unknown>;
  const allowed = ["agentVersionId", "harnessProfileId", "architecture", "expiresInSeconds"];
  if (Object.keys(body).some((key) => !allowed.includes(key))) throw invalid("agent has unknown fields");
  if (typeof body.agentVersionId !== "string" || !UUID.test(body.agentVersionId)) throw invalid("agent.agentVersionId must be a UUID");
  if (typeof body.harnessProfileId !== "string" || !UUID.test(body.harnessProfileId)) throw invalid("agent.harnessProfileId must be a UUID");
  const architecture = body.architecture ?? "amd64";
  if (architecture !== "amd64" && architecture !== "arm64") throw invalid("agent.architecture must be amd64 or arm64");
  const request: AgentRunRequest = { agentVersionId: body.agentVersionId, harnessProfileId: body.harnessProfileId, architecture };
  if (body.expiresInSeconds !== undefined) {
    if (!Number.isSafeInteger(body.expiresInSeconds) || (body.expiresInSeconds as number) < 300 || (body.expiresInSeconds as number) > 7200) throw invalid("agent.expiresInSeconds must be 300 through 7200");
    request.expiresInSeconds = body.expiresInSeconds as number;
  }
  return request;
}

/**
 * Admits an Agent Run: proves the Agent, Harness and template are compatible,
 * creates the Run's Sandbox as the requester (whose session is live now), and
 * hands the Run to the Agent Run controller. Every step is idempotent, so a
 * retried request with the same Idempotency-Key resumes where it stopped.
 */
export class AgentRunAdmission {
  constructor(private readonly database: Database, private readonly sandboxes: SandboxService) {}

  async admit(principal: SandboxPrincipal, run: Run, request: AgentRunRequest): Promise<void> {
    if (run.proofClass !== "sandbox") throw invalid("an Agent Run must use proofClass sandbox");
    if (run.requestedBy !== principal.userId) throw new RunHttpError("permission_denied", "only the Run requester can start its Agent");
    const started = await this.database.query<{ sandbox_id: string }>("SELECT sandbox_id FROM agent_run_executions WHERE run_id=$1", [run.id]);
    if (started.rows[0]) return;
    if (run.status !== "queued") throw new RunHttpError("run_terminal", "Run is no longer queued");

    const agent = await this.database.query<{ document: Record<string, unknown> }>("SELECT document FROM agent_versions WHERE id=$1 AND workspace_id=$2", [request.agentVersionId, run.workspaceId]);
    const document = agent.rows[0]?.document;
    if (!document) throw invalid("the AgentVersion was not found in this Workspace");
    const template = record(document.template), repository = record(document.repository);
    const templateVersionId = text(template.versionId), repositoryUrl = text(repository.url), commit = text(repository.commit);
    if (!templateVersionId || !repositoryUrl || !/^(?:[0-9a-f]{40}|[0-9a-f]{64})$/.test(commit)) throw invalid("the AgentVersion does not pin a template and repository commit");

    if (!await this.compatible(run, request)) throw invalid("the Agent, HarnessProfile, template and Run outputs are not compatible; the Run outputs must be exactly the Agent's output contract");

    let version;
    try { version = (await this.sandboxes.getVersion(principal, templateVersionId)).version; }
    catch (error) { if (error instanceof SandboxHttpError) throw invalid("the Agent's Sandbox template is not available to you"); throw error; }
    const repositories = version.manifest.spec.repositories ?? [];
    const sources = repositories.map((entry) => {
      if (normalizeRepository(entry.url) !== normalizeRepository(repositoryUrl)) throw invalid(`the Sandbox template repository ${entry.name} is not the Agent's repository`);
      return { repository: entry.name, commit };
    });
    const maximum = version.manifest.spec.expiresInSeconds;
    const expiresInSeconds = Math.min(request.expiresInSeconds ?? maximum, maximum);
    let sandboxId: string;
    try {
      const created = await this.sandboxes.createSandbox(principal, run.workspaceId, `agent-run-${run.id}`, {
        template: { name: version.name, version: version.version }, architecture: request.architecture, allocationMode: "direct",
        expiresInSeconds, sources, approvedNonSensitive: true,
      }) as { sandbox: { id: string } };
      sandboxId = created.sandbox.id;
    } catch (error) {
      if (error instanceof SandboxHttpError) throw invalid(`the Run's Sandbox could not be created: ${error.message}`);
      throw error;
    }
    const outcome = await this.database.query<{ outcome: string }>("SELECT agent_run_start_execution($1,$2,$3,$4,$5,$6,$7) AS outcome",
      [run.id, run.workspaceId, request.agentVersionId, request.harnessProfileId, sandboxId, principal.userId, principal.sessionId]);
    const result = outcome.rows[0]?.outcome;
    if (result !== "accepted") throw invalid(`the Agent Run could not be started (${result ?? "unknown"})`);
  }

  /** Runs the admission check and rolls it back, so nothing is frozen before the Sandbox exists. */
  private async compatible(run: Run, request: AgentRunRequest): Promise<boolean> {
    const client = await this.database.connect();
    try {
      await client.query("BEGIN");
      const result = await client.query<{ accepted: boolean }>("SELECT agent_run_enqueue($1,$2,$3,$4) AS accepted", [run.id, run.workspaceId, request.agentVersionId, request.harnessProfileId]);
      return result.rows[0]?.accepted === true;
    } finally {
      await client.query("ROLLBACK").catch(() => undefined);
      client.release();
    }
  }
}

function normalizeRepository(url: string) { return url.trim().toLowerCase().replace(/\.git$/, "").replace(/\/$/, ""); }
function record(value: unknown): Record<string, unknown> { return value && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : {}; }
function text(value: unknown) { return typeof value === "string" ? value : ""; }
function invalid(message: string) { return new RunHttpError("invalid_request", message); }

/** Reads the stored bytes of a database-backed Artifact (Agent Run output or synthetic). */
export function artifactContentReader(database: Database) {
  return async (artifactId: string): Promise<Buffer | undefined> => {
    const result = await database.query<{ content: Buffer }>(
      "SELECT content FROM agent_run_artifact_blobs WHERE artifact_id=$1 UNION ALL SELECT content FROM synthetic_artifact_blobs WHERE artifact_id=$1 LIMIT 1", [artifactId]);
    return result.rows[0]?.content;
  };
}

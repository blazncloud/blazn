import type { Database } from "./db.js";

/** The controller's lease on one Run. Every store call is fenced by it. */
export interface AgentRunLease { runId: string; workerId: string; leaseToken: string }

export interface AgentRunExecution {
  sandboxId: string;
  sandboxState: string;
  sandboxExpiresAt: string;
  architecture: "amd64" | "arm64";
  nodeId?: string;
  outboxSequence: number;
  runStatus: string;
  runVersion: number;
  instructions: string;
  purpose: string;
  repositoryDestination?: string;
}

export interface AgentRunMessageClaim { messageId: string; claimId: string; ordinal: number; kind: string; content: string; resumed: boolean }
export type AgentRunGrantKind = "exec" | "upload" | "download";

export interface AgentRunExecutionStore {
  /** Undefined means the lease is no longer held (expired, fenced, or the Run ended). */
  execution(lease: AgentRunLease): Promise<AgentRunExecution | undefined>;
  issueGrant(lease: AgentRunLease, grantId: string, tokenHash: string, kind: AgentRunGrantKind, ttlSeconds: number): Promise<boolean>;
  claimMessage(lease: AgentRunLease, leaseSeconds: number): Promise<AgentRunMessageClaim | undefined>;
  deliverMessage(lease: AgentRunLease, messageId: string, claimId: string): Promise<boolean>;
  recordReply(lease: AgentRunLease, sequence: number, parentMessageId: string | undefined, content: string): Promise<string | undefined>;
  /** A null type only advances the consumed harness sequence. */
  recordEvent(lease: AgentRunLease, sequence: number, type: string | null, payload: Record<string, unknown>): Promise<boolean>;
  recordArtifact(lease: AgentRunLease, name: "patch" | "summary", content: Buffer): Promise<string | undefined>;
  releaseSandboxes(limit: number): Promise<number>;
}

export class PgAgentRunExecutionStore implements AgentRunExecutionStore {
  constructor(private readonly database: Database) {}

  async execution(lease: AgentRunLease) {
    const result = await this.database.query("SELECT * FROM agent_run_controller_execution($1,$2,$3)", args(lease));
    const row = result.rows[0];
    if (!row) return undefined;
    const execution: AgentRunExecution = {
      sandboxId: row.sandbox_id, sandboxState: row.sandbox_state, sandboxExpiresAt: new Date(row.sandbox_expires_at).toISOString(),
      architecture: row.architecture, outboxSequence: Number(row.outbox_sequence), runStatus: row.run_status, runVersion: Number(row.run_version),
      instructions: row.instructions ?? "", purpose: row.purpose ?? "",
    };
    if (row.node_id) execution.nodeId = row.node_id;
    if (row.repository_destination) execution.repositoryDestination = row.repository_destination;
    return execution;
  }

  async issueGrant(lease: AgentRunLease, grantId: string, tokenHash: string, kind: AgentRunGrantKind, ttlSeconds: number) {
    const result = await this.database.query<{ issued: boolean }>("SELECT agent_run_controller_issue_grant($1,$2,$3,$4,$5,$6,$7) AS issued", [...args(lease), grantId, tokenHash, kind, ttlSeconds]);
    return result.rows[0]?.issued === true;
  }

  async claimMessage(lease: AgentRunLease, leaseSeconds: number) {
    const result = await this.database.query("SELECT * FROM agent_run_controller_claim_message($1,$2,$3,$4)", [...args(lease), leaseSeconds]);
    const row = result.rows[0];
    return row ? { messageId: row.message_id, claimId: row.claim_id, ordinal: Number(row.ordinal), kind: row.kind, content: row.content, resumed: row.resumed === true } : undefined;
  }

  async deliverMessage(lease: AgentRunLease, messageId: string, claimId: string) {
    const result = await this.database.query<{ delivered: boolean }>("SELECT agent_run_controller_deliver_message($1,$2,$3,$4,$5) AS delivered", [...args(lease), messageId, claimId]);
    return result.rows[0]?.delivered === true;
  }

  async recordReply(lease: AgentRunLease, sequence: number, parentMessageId: string | undefined, content: string) {
    const result = await this.database.query<{ id: string | null }>("SELECT agent_run_controller_record_reply($1,$2,$3,$4,$5,$6) AS id", [...args(lease), sequence, parentMessageId ?? null, content]);
    return result.rows[0]?.id ?? undefined;
  }

  async recordEvent(lease: AgentRunLease, sequence: number, type: string | null, payload: Record<string, unknown>) {
    const result = await this.database.query<{ recorded: boolean }>("SELECT agent_run_controller_record_event($1,$2,$3,$4,$5,$6) AS recorded", [...args(lease), sequence, type, JSON.stringify(payload)]);
    return result.rows[0]?.recorded === true;
  }

  async recordArtifact(lease: AgentRunLease, name: "patch" | "summary", content: Buffer) {
    const result = await this.database.query<{ id: string | null }>("SELECT agent_run_controller_record_artifact($1,$2,$3,$4,$5) AS id", [...args(lease), name, content]);
    return result.rows[0]?.id ?? undefined;
  }

  async releaseSandboxes(limit: number) {
    const result = await this.database.query<{ released: number }>("SELECT agent_run_controller_release_sandboxes($1) AS released", [limit]);
    return Number(result.rows[0]?.released ?? 0);
  }
}

function args(lease: AgentRunLease) { return [lease.runId, lease.workerId, lease.leaseToken]; }

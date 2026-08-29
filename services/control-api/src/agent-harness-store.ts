import type { PoolClient, QueryResultRow } from "pg";
import type { Database } from "./db.js";
import type { IdempotencyReceipt } from "./workspace-store.js";
import type { Agent, AgentHarnessAccess, AgentVersion, HarnessDefinition, HarnessProfile, HarnessVersion, JsonDocument } from "./agent-harness-types.js";

export class AgentNameConflictError extends Error {}
export class AgentVersionConflictError extends Error {}
export class DefinitionKindConflictError extends Error {}
export class HarnessVersionConflictError extends Error {}
export class ProfileNameConflictError extends Error {}
export class ProfileRevisionConflictError extends Error {}
export class IdentityConflictError extends Error {}

export interface AgentHarnessTransaction {
  lockIdempotency(principalId: string, operation: string, key: string): Promise<void>;
  getIdempotency(principalId: string, operation: string, key: string): Promise<IdempotencyReceipt | undefined>;
  putIdempotency(principalId: string, operation: string, key: string, receipt: IdempotencyReceipt): Promise<void>;
  getAccess(workspaceId: string, userId: string, lock?: boolean): Promise<AgentHarnessAccess | undefined>;

  createAgent(input: { id: string; workspaceId: string; ownerId: string; name: string; tags: string[]; createdBy: string }): Promise<Agent>;
  getAgent(workspaceId: string, agentId: string, lock?: boolean): Promise<Agent | undefined>;
  listAgents(workspaceId: string, cursor: string): Promise<{ items: Agent[]; nextCursor: string | null }>;
  insertAgentVersion(input: { id: string; agentId: string; workspaceId: string; version: number; digest: string; document: JsonDocument; createdBy: string }): Promise<AgentVersion>;
  setAgentCurrentVersion(agent: Agent, versionId: string): Promise<Agent | undefined>;
  getAgentVersion(workspaceId: string, agentId: string, versionId: string): Promise<AgentVersion | undefined>;
  listAgentVersions(workspaceId: string, agentId: string, cursor: string): Promise<{ items: AgentVersion[]; nextCursor: string | null }>;
  maxAgentVersionNumber(agentId: string): Promise<number>;

  createHarnessDefinition(input: { id: string; workspaceId: string; kind: string; status: string; resourceVersion: number; document: JsonDocument; createdBy: string }): Promise<HarnessDefinition>;
  getHarnessDefinition(workspaceId: string, definitionId: string, lock?: boolean): Promise<HarnessDefinition | undefined>;
  listHarnessDefinitions(workspaceId: string, cursor: string): Promise<{ items: HarnessDefinition[]; nextCursor: string | null }>;
  insertHarnessVersion(input: { id: string; definitionId: string; workspaceId: string; version: string; digest: string; document: JsonDocument; createdBy: string }): Promise<HarnessVersion>;
  getHarnessVersion(workspaceId: string, versionId: string): Promise<HarnessVersion | undefined>;
  listHarnessVersions(workspaceId: string, definitionId: string, cursor: string): Promise<{ items: HarnessVersion[]; nextCursor: string | null }>;

  createHarnessProfile(input: { id: string; workspaceId: string; name: string; harnessVersionId: string; status: string; resourceVersion: number; digest: string; document: JsonDocument; createdBy: string }): Promise<HarnessProfile>;
  reviseHarnessProfile(profile: HarnessProfile, input: { name: string; harnessVersionId: string; status: string; resourceVersion: number; digest: string; document: JsonDocument; revisedBy: string }): Promise<HarnessProfile | undefined>;
  insertHarnessProfileRevision(input: { id: string; profileId: string; workspaceId: string; resourceVersion: number; digest: string; document: JsonDocument; createdBy: string }): Promise<void>;
  getHarnessProfile(workspaceId: string, profileId: string, lock?: boolean): Promise<HarnessProfile | undefined>;
  listHarnessProfiles(workspaceId: string, cursor: string): Promise<{ items: HarnessProfile[]; nextCursor: string | null }>;

  getTemplateVersionDigest(workspaceId: string, templateVersionId: string): Promise<string | undefined>;
  insertAudit(id: string, workspaceId: string, actorUserId: string, type: string, payload: unknown): Promise<void>;
}
export interface AgentHarnessStore { transaction<T>(action: (transaction: AgentHarnessTransaction) => Promise<T>): Promise<T> }

export class PgAgentHarnessStore implements AgentHarnessStore {
  constructor(private readonly database: Database) {}
  async transaction<T>(action: (transaction: AgentHarnessTransaction) => Promise<T>): Promise<T> {
    const client = await this.database.connect();
    try { await client.query("BEGIN"); const result = await action(new PgAgentHarnessTransaction(client)); await client.query("COMMIT"); return result; }
    catch (error) { await client.query("ROLLBACK"); throw error; }
    finally { client.release(); }
  }
}

const agentColumns = "id,workspace_id,owner_id,name,tags,status,current_version_id,version,created_at,updated_at";
const agentVersionColumns = "id,agent_id,workspace_id,version,digest,document,created_by,created_at";
const definitionColumns = "id,workspace_id,kind,status,resource_version,document,created_at,updated_at";
const harnessVersionColumns = "id,definition_id,workspace_id,version,digest,document,created_by,created_at";
const profileColumns = "id,workspace_id,name,harness_version_id,status,resource_version,digest,document,created_at,updated_at";

class PgAgentHarnessTransaction implements AgentHarnessTransaction {
  constructor(private readonly client: PoolClient) {}
  async lockIdempotency(principalId: string, operation: string, key: string) { await this.client.query("SELECT pg_advisory_xact_lock(hashtextextended($1,0))", [`${principalId}\n${operation}\n${key}`]); }
  async getIdempotency(principalId: string, operation: string, key: string) {
    const result = await this.client.query("SELECT workspace_id,target_key,request_digest,response_status,response_body FROM workspace_idempotency_receipts WHERE principal_id=$1 AND operation=$2 AND idempotency_key=$3", [principalId, operation, key]);
    const row = result.rows[0];
    return row ? { workspaceId: row.workspace_id, targetKey: row.target_key, requestDigest: row.request_digest.trim(), responseStatus: row.response_status, responseBody: row.response_body } : undefined;
  }
  async putIdempotency(principalId: string, operation: string, key: string, receipt: IdempotencyReceipt) {
    await this.client.query("INSERT INTO workspace_idempotency_receipts(principal_id,workspace_id,operation,target_key,idempotency_key,request_digest,response_status,response_body) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", [principalId, receipt.workspaceId, operation, receipt.targetKey, key, receipt.requestDigest, receipt.responseStatus, receipt.responseBody]);
  }
  async getAccess(workspaceId: string, userId: string, lock = false): Promise<AgentHarnessAccess | undefined> {
    const suffix = lock ? " FOR SHARE OF w,m" : "";
    const result = await this.client.query(`SELECT w.status AS workspace_status,m.role FROM workspaces w JOIN workspace_memberships m ON m.workspace_id=w.id AND m.user_id=$2 AND m.status='active' WHERE w.id=$1${suffix}`, [workspaceId, userId]);
    const row = result.rows[0];
    return row ? { workspaceStatus: row.workspace_status, role: row.role } : undefined;
  }

  async createAgent(input: { id: string; workspaceId: string; ownerId: string; name: string; tags: string[]; createdBy: string }): Promise<Agent> {
    try {
      const result = await this.client.query(`INSERT INTO agents(id,workspace_id,owner_id,name,tags,status,version,created_by) VALUES($1,$2,$3,$4,$5::jsonb,'active',1,$6) RETURNING ${agentColumns}`, [input.id, input.workspaceId, input.ownerId, input.name, JSON.stringify(input.tags), input.createdBy]);
      return agentRow(result.rows[0]!);
    } catch (error) { throw mapConflict(error, "agents_workspace_id_name_key", AgentNameConflictError, "agents_pkey", IdentityConflictError); }
  }
  async getAgent(workspaceId: string, agentId: string, lock = false): Promise<Agent | undefined> {
    const result = await this.client.query(`SELECT ${agentColumns} FROM agents WHERE workspace_id=$1 AND id=$2${lock ? " FOR UPDATE" : ""}`, [workspaceId, agentId]);
    return result.rows[0] ? agentRow(result.rows[0]) : undefined;
  }
  async listAgents(workspaceId: string, cursor = ""): Promise<{ items: Agent[]; nextCursor: string | null }> {
    const result = await this.client.query(`SELECT ${agentColumns} FROM agents WHERE workspace_id=$1 AND ($2='' OR id::text>$2) ORDER BY id LIMIT 101`, [workspaceId, cursor]);
    const items = result.rows.slice(0, 100).map(agentRow);
    return { items, nextCursor: result.rows.length > 100 ? items.at(-1)?.id ?? null : null };
  }
  async insertAgentVersion(input: { id: string; agentId: string; workspaceId: string; version: number; digest: string; document: JsonDocument; createdBy: string }): Promise<AgentVersion> {
    try {
      const result = await this.client.query(`INSERT INTO agent_versions(id,agent_id,workspace_id,version,digest,document,created_by) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7) RETURNING ${agentVersionColumns}`, [input.id, input.agentId, input.workspaceId, input.version, input.digest, JSON.stringify(input.document), input.createdBy]);
      return agentVersionRow(result.rows[0]!);
    } catch (error) { throw mapConflict(error, "agent_versions_agent_id_version_key", AgentVersionConflictError, "agent_versions_pkey", IdentityConflictError, "agent_versions_agent_id_digest_key", AgentVersionConflictError); }
  }
  async setAgentCurrentVersion(agent: Agent, versionId: string): Promise<Agent | undefined> {
    const result = await this.client.query(`UPDATE agents SET current_version_id=$3,version=version+1,updated_at=now() WHERE workspace_id=$1 AND id=$2 AND version=$4 RETURNING ${agentColumns}`, [agent.workspaceId, agent.id, versionId, agent.version]);
    return result.rows[0] ? agentRow(result.rows[0]) : undefined;
  }
  async getAgentVersion(workspaceId: string, agentId: string, versionId: string): Promise<AgentVersion | undefined> {
    const result = await this.client.query(`SELECT ${agentVersionColumns} FROM agent_versions WHERE workspace_id=$1 AND agent_id=$2 AND id=$3`, [workspaceId, agentId, versionId]);
    return result.rows[0] ? agentVersionRow(result.rows[0]) : undefined;
  }
  async listAgentVersions(workspaceId: string, agentId: string, cursor = ""): Promise<{ items: AgentVersion[]; nextCursor: string | null }> {
    const after = cursor === "" ? 0 : Number(cursor);
    const result = await this.client.query(`SELECT ${agentVersionColumns} FROM agent_versions WHERE workspace_id=$1 AND agent_id=$2 AND version>$3 ORDER BY version LIMIT 101`, [workspaceId, agentId, after]);
    const items = result.rows.slice(0, 100).map(agentVersionRow);
    return { items, nextCursor: result.rows.length > 100 ? String(items.at(-1)?.version ?? "") : null };
  }
  async maxAgentVersionNumber(agentId: string): Promise<number> {
    const result = await this.client.query("SELECT COALESCE(MAX(version),0) AS max FROM agent_versions WHERE agent_id=$1", [agentId]);
    return Number(result.rows[0]?.max ?? 0);
  }

  async createHarnessDefinition(input: { id: string; workspaceId: string; kind: string; status: string; resourceVersion: number; document: JsonDocument; createdBy: string }): Promise<HarnessDefinition> {
    try {
      const result = await this.client.query(`INSERT INTO harness_definitions(id,workspace_id,kind,status,resource_version,document,created_by) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7) RETURNING ${definitionColumns}`, [input.id, input.workspaceId, input.kind, input.status, input.resourceVersion, JSON.stringify(input.document), input.createdBy]);
      return definitionRow(result.rows[0]!);
    } catch (error) { throw mapConflict(error, "harness_definitions_workspace_id_kind_key", DefinitionKindConflictError, "harness_definitions_pkey", IdentityConflictError); }
  }
  async getHarnessDefinition(workspaceId: string, definitionId: string, lock = false): Promise<HarnessDefinition | undefined> {
    const result = await this.client.query(`SELECT ${definitionColumns} FROM harness_definitions WHERE workspace_id=$1 AND id=$2${lock ? " FOR SHARE" : ""}`, [workspaceId, definitionId]);
    return result.rows[0] ? definitionRow(result.rows[0]) : undefined;
  }
  async listHarnessDefinitions(workspaceId: string, cursor = ""): Promise<{ items: HarnessDefinition[]; nextCursor: string | null }> {
    const result = await this.client.query(`SELECT ${definitionColumns} FROM harness_definitions WHERE workspace_id=$1 AND ($2='' OR id::text>$2) ORDER BY id LIMIT 101`, [workspaceId, cursor]);
    const items = result.rows.slice(0, 100).map(definitionRow);
    return { items, nextCursor: result.rows.length > 100 ? items.at(-1)?.id ?? null : null };
  }
  async insertHarnessVersion(input: { id: string; definitionId: string; workspaceId: string; version: string; digest: string; document: JsonDocument; createdBy: string }): Promise<HarnessVersion> {
    try {
      const result = await this.client.query(`INSERT INTO harness_versions(id,definition_id,workspace_id,version,digest,document,created_by) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7) RETURNING ${harnessVersionColumns}`, [input.id, input.definitionId, input.workspaceId, input.version, input.digest, JSON.stringify(input.document), input.createdBy]);
      return harnessVersionRow(result.rows[0]!);
    } catch (error) { throw mapConflict(error, "harness_versions_definition_id_version_key", HarnessVersionConflictError, "harness_versions_pkey", IdentityConflictError, "harness_versions_definition_id_digest_key", HarnessVersionConflictError); }
  }
  async getHarnessVersion(workspaceId: string, versionId: string): Promise<HarnessVersion | undefined> {
    const result = await this.client.query(`SELECT ${harnessVersionColumns} FROM harness_versions WHERE workspace_id=$1 AND id=$2`, [workspaceId, versionId]);
    return result.rows[0] ? harnessVersionRow(result.rows[0]) : undefined;
  }
  async listHarnessVersions(workspaceId: string, definitionId: string, cursor = ""): Promise<{ items: HarnessVersion[]; nextCursor: string | null }> {
    const result = await this.client.query(`SELECT ${harnessVersionColumns} FROM harness_versions WHERE workspace_id=$1 AND definition_id=$2 AND ($3='' OR id::text>$3) ORDER BY id LIMIT 101`, [workspaceId, definitionId, cursor]);
    const items = result.rows.slice(0, 100).map(harnessVersionRow);
    return { items, nextCursor: result.rows.length > 100 ? items.at(-1)?.id ?? null : null };
  }

  async createHarnessProfile(input: { id: string; workspaceId: string; name: string; harnessVersionId: string; status: string; resourceVersion: number; digest: string; document: JsonDocument; createdBy: string }): Promise<HarnessProfile> {
    try {
      const result = await this.client.query(`INSERT INTO harness_profiles(id,workspace_id,name,harness_version_id,status,resource_version,digest,document,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9) RETURNING ${profileColumns}`, [input.id, input.workspaceId, input.name, input.harnessVersionId, input.status, input.resourceVersion, input.digest, JSON.stringify(input.document), input.createdBy]);
      return profileRow(result.rows[0]!);
    } catch (error) { throw mapConflict(error, "harness_profiles_workspace_id_name_key", ProfileNameConflictError, "harness_profiles_pkey", IdentityConflictError); }
  }
  async reviseHarnessProfile(profile: HarnessProfile, input: { name: string; harnessVersionId: string; status: string; resourceVersion: number; digest: string; document: JsonDocument; revisedBy: string }): Promise<HarnessProfile | undefined> {
    try {
      const result = await this.client.query(`UPDATE harness_profiles SET name=$3,harness_version_id=$4,status=$5,resource_version=$6,digest=$7,document=$8::jsonb,updated_at=now() WHERE workspace_id=$1 AND id=$2 AND resource_version=$9 RETURNING ${profileColumns}`, [profile.workspaceId, profile.id, input.name, input.harnessVersionId, input.status, input.resourceVersion, input.digest, JSON.stringify(input.document), profile.resourceVersion]);
      return result.rows[0] ? profileRow(result.rows[0]) : undefined;
    } catch (error) { throw mapConflict(error, "harness_profiles_workspace_id_name_key", ProfileNameConflictError); }
  }
  async insertHarnessProfileRevision(input: { id: string; profileId: string; workspaceId: string; resourceVersion: number; digest: string; document: JsonDocument; createdBy: string }): Promise<void> {
    try {
      await this.client.query("INSERT INTO harness_profile_revisions(id,profile_id,workspace_id,resource_version,digest,document,created_by) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7)", [input.id, input.profileId, input.workspaceId, input.resourceVersion, input.digest, JSON.stringify(input.document), input.createdBy]);
    } catch (error) { throw mapConflict(error, "harness_profile_revisions_profile_id_resource_version_key", ProfileRevisionConflictError); }
  }
  async getHarnessProfile(workspaceId: string, profileId: string, lock = false): Promise<HarnessProfile | undefined> {
    const result = await this.client.query(`SELECT ${profileColumns} FROM harness_profiles WHERE workspace_id=$1 AND id=$2${lock ? " FOR UPDATE" : ""}`, [workspaceId, profileId]);
    return result.rows[0] ? profileRow(result.rows[0]) : undefined;
  }
  async listHarnessProfiles(workspaceId: string, cursor = ""): Promise<{ items: HarnessProfile[]; nextCursor: string | null }> {
    const result = await this.client.query(`SELECT ${profileColumns} FROM harness_profiles WHERE workspace_id=$1 AND ($2='' OR id::text>$2) ORDER BY id LIMIT 101`, [workspaceId, cursor]);
    const items = result.rows.slice(0, 100).map(profileRow);
    return { items, nextCursor: result.rows.length > 100 ? items.at(-1)?.id ?? null : null };
  }

  async getTemplateVersionDigest(workspaceId: string, templateVersionId: string): Promise<string | undefined> {
    const result = await this.client.query("SELECT content_digest FROM sandbox_template_versions WHERE workspace_id=$1 AND id=$2", [workspaceId, templateVersionId]);
    return result.rows[0]?.content_digest ?? undefined;
  }
  async insertAudit(id: string, workspaceId: string, actorUserId: string, type: string, payload: unknown) {
    await this.client.query("INSERT INTO workspace_audit_events(id,workspace_id,actor_user_id,event_type,payload) VALUES($1,$2,$3,$4,$5)", [id, workspaceId, actorUserId, type, JSON.stringify(payload ?? {})]);
  }
}

function mapConflict(error: unknown, ...pairs: (string | (new () => Error))[]): unknown {
  if (error && typeof error === "object" && "code" in error && (error as { code?: string }).code === "23505" && "constraint" in error) {
    const constraint = (error as { constraint?: string }).constraint;
    for (let index = 0; index + 1 < pairs.length; index += 2) {
      if (pairs[index] === constraint) return new (pairs[index + 1] as new () => Error)();
    }
  }
  return error;
}
function tags(value: unknown): string[] { return Array.isArray(value) && value.every((item) => typeof item === "string") ? value : []; }
function iso(value: unknown): string { return value instanceof Date ? value.toISOString() : String(value); }
function agentRow(row: QueryResultRow): Agent {
  return { id: row.id, workspaceId: row.workspace_id, ownerId: row.owner_id, name: row.name, tags: tags(row.tags), status: row.status, currentVersionId: row.current_version_id ?? null, version: Number(row.version), createdAt: iso(row.created_at), updatedAt: iso(row.updated_at) };
}
function agentVersionRow(row: QueryResultRow): AgentVersion {
  return { id: row.id, agentId: row.agent_id, workspaceId: row.workspace_id, version: Number(row.version), digest: row.digest, document: row.document, createdBy: row.created_by, createdAt: iso(row.created_at) };
}
function definitionRow(row: QueryResultRow): HarnessDefinition {
  return { id: row.id, workspaceId: row.workspace_id, kind: row.kind, status: row.status, resourceVersion: Number(row.resource_version), document: row.document, createdAt: iso(row.created_at), updatedAt: iso(row.updated_at) };
}
function harnessVersionRow(row: QueryResultRow): HarnessVersion {
  return { id: row.id, definitionId: row.definition_id, workspaceId: row.workspace_id, version: row.version, digest: row.digest, document: row.document, createdBy: row.created_by, createdAt: iso(row.created_at) };
}
function profileRow(row: QueryResultRow): HarnessProfile {
  return { id: row.id, workspaceId: row.workspace_id, name: row.name, harnessVersionId: row.harness_version_id, status: row.status, resourceVersion: Number(row.resource_version), digest: row.digest, document: row.document, createdAt: iso(row.created_at), updatedAt: iso(row.updated_at) };
}

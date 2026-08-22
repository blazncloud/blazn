import type { PoolClient, QueryResultRow } from "pg";
import type { Database } from "./db.js";
import type { IdempotencyReceipt } from "./workspace-store.js";
import type { Project, ProjectAccess, ProjectStatus } from "./project-types.js";

export interface ProjectTransaction {
  lockIdempotency(principalId: string, operation: string, key: string): Promise<void>;
  getIdempotency(principalId: string, operation: string, key: string): Promise<IdempotencyReceipt | undefined>;
  putIdempotency(principalId: string, operation: string, key: string, receipt: IdempotencyReceipt): Promise<void>;
  getWorkspaceAccess(workspaceId: string, userId: string, lock?: boolean): Promise<ProjectAccess | undefined>;
  createProject(project: Omit<Project, "createdAt" | "updatedAt">): Promise<Project>;
  getProject(workspaceId: string, projectId: string, lock?: boolean): Promise<Project | undefined>;
  listProjects(workspaceId: string, status: ProjectStatus | "all", cursor?: string): Promise<{ items: Project[]; nextCursor: string | null }>;
  updateProject(workspaceId: string, projectId: string, expectedVersion: number, changes: { name?: string; description?: string; status?: ProjectStatus }): Promise<Project | undefined>;
  insertAudit(id: string, workspaceId: string, actorUserId: string, type: string, payload: unknown): Promise<void>;
}

export interface ProjectStore {
  transaction<T>(action: (transaction: ProjectTransaction) => Promise<T>): Promise<T>;
}

export class PgProjectStore implements ProjectStore {
  constructor(private readonly database: Database) {}

  async transaction<T>(action: (transaction: ProjectTransaction) => Promise<T>): Promise<T> {
    const client = await this.database.connect();
    try {
      await client.query("BEGIN");
      const result = await action(new PgProjectTransaction(client));
      await client.query("COMMIT");
      return result;
    } catch (error) {
      await client.query("ROLLBACK");
      throw error;
    } finally {
      client.release();
    }
  }
}

class PgProjectTransaction implements ProjectTransaction {
  constructor(private readonly client: PoolClient) {}

  async lockIdempotency(principalId: string, operation: string, key: string): Promise<void> {
    await this.client.query("SELECT pg_advisory_xact_lock(hashtextextended($1,0))", [`${principalId}\n${operation}\n${key}`]);
  }

  async getIdempotency(principalId: string, operation: string, key: string): Promise<IdempotencyReceipt | undefined> {
    const result = await this.client.query("SELECT workspace_id,target_key,request_digest,response_status,response_body FROM workspace_idempotency_receipts WHERE principal_id=$1 AND operation=$2 AND idempotency_key=$3", [principalId, operation, key]);
    const row = result.rows[0];
    return row ? { workspaceId: row.workspace_id, targetKey: row.target_key, requestDigest: row.request_digest.trim(), responseStatus: row.response_status, responseBody: row.response_body } : undefined;
  }

  async putIdempotency(principalId: string, operation: string, key: string, receipt: IdempotencyReceipt): Promise<void> {
    await this.client.query("INSERT INTO workspace_idempotency_receipts(principal_id,workspace_id,operation,target_key,idempotency_key,request_digest,response_status,response_body) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", [principalId, receipt.workspaceId, operation, receipt.targetKey, key, receipt.requestDigest, receipt.responseStatus, receipt.responseBody]);
  }

  async getWorkspaceAccess(workspaceId: string, userId: string, lock = false): Promise<ProjectAccess | undefined> {
    const suffix = lock ? " FOR UPDATE OF w" : "";
    const result = await this.client.query(`SELECT w.status AS workspace_status,m.role FROM workspaces w JOIN workspace_memberships m ON m.workspace_id=w.id WHERE w.id=$1 AND m.user_id=$2 AND m.status='active'${suffix}`, [workspaceId, userId]);
    const row = result.rows[0];
    return row ? { workspaceStatus: row.workspace_status, role: row.role } : undefined;
  }

  async createProject(project: Omit<Project, "createdAt" | "updatedAt">): Promise<Project> {
    const result = await this.client.query(`INSERT INTO projects(id,workspace_id,slug,kind,name,description,status,version,created_by)
      VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING *`, [project.id, project.workspaceId, project.slug, project.kind, project.name, project.description, project.status, project.version, project.createdBy]);
    return projectRow(result.rows[0]!);
  }

  async getProject(workspaceId: string, projectId: string, lock = false): Promise<Project | undefined> {
    const result = await this.client.query(`SELECT * FROM projects WHERE workspace_id=$1 AND id=$2${lock ? " FOR UPDATE" : ""}`, [workspaceId, projectId]);
    return result.rows[0] ? projectRow(result.rows[0]) : undefined;
  }

  async listProjects(workspaceId: string, status: ProjectStatus | "all", cursor = ""): Promise<{ items: Project[]; nextCursor: string | null }> {
    const result = await this.client.query(`SELECT * FROM projects WHERE workspace_id=$1 AND ($2='all' OR status=$2) AND ($3='' OR id::text>$3) ORDER BY id LIMIT 101`, [workspaceId, status, cursor]);
    const items = result.rows.slice(0, 100).map(projectRow);
    return { items, nextCursor: result.rows.length > 100 ? items.at(-1)?.id ?? null : null };
  }

  async updateProject(workspaceId: string, projectId: string, expectedVersion: number, changes: { name?: string; description?: string; status?: ProjectStatus }): Promise<Project | undefined> {
    const result = await this.client.query(`UPDATE projects SET name=COALESCE($3,name),description=COALESCE($4,description),status=COALESCE($5,status),version=version+1,updated_at=now()
      WHERE workspace_id=$1 AND id=$2 AND version=$6 RETURNING *`, [workspaceId, projectId, changes.name ?? null, changes.description ?? null, changes.status ?? null, expectedVersion]);
    return result.rows[0] ? projectRow(result.rows[0]) : undefined;
  }

  async insertAudit(id: string, workspaceId: string, actorUserId: string, type: string, payload: unknown): Promise<void> {
    await this.client.query("INSERT INTO workspace_audit_events(id,workspace_id,actor_user_id,event_type,payload) VALUES($1,$2,$3,$4,$5)", [id, workspaceId, actorUserId, type, payload]);
  }
}

function projectRow(row: QueryResultRow): Project {
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    slug: row.slug,
    kind: row.kind,
    name: row.name,
    description: row.description,
    status: row.status,
    version: Number(row.version),
    createdBy: row.created_by,
    createdAt: timestamp(row.created_at),
    updatedAt: timestamp(row.updated_at),
  };
}

function timestamp(value: Date | string): string {
  return value instanceof Date ? value.toISOString() : new Date(value).toISOString();
}

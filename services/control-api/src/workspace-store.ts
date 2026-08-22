import type { PoolClient, QueryResultRow } from "pg";
import type { Database } from "./db.js";
import type { Invitation, Membership, Workspace, WorkspacePrincipal, WorkspaceRole } from "./workspace-types.js";

export interface IdempotencyReceipt {
  workspaceId: string;
  targetKey: string;
  requestDigest: string;
  responseStatus: number;
  responseBody: unknown;
}

export interface WorkspaceAuditEvent {
  id: string;
  type: string;
  payload: unknown;
  createdAt: string;
}

export interface WorkspaceTransaction {
  lockIdempotency(principalId: string, operation: string, key: string): Promise<void>;
  getIdempotency(principalId: string, operation: string, key: string): Promise<IdempotencyReceipt | undefined>;
  putIdempotency(principalId: string, operation: string, key: string, receipt: IdempotencyReceipt): Promise<void>;
  createWorkspace(id: string, slug: string, name: string, principal: WorkspacePrincipal): Promise<Workspace>;
  lockWorkspace(workspaceId: string): Promise<{ createdBy: string; version: number } | undefined>;
  getWorkspace(workspaceId: string, userId: string): Promise<Workspace | undefined>;
  getMembership(workspaceId: string, userId: string, lock?: boolean): Promise<{ role: WorkspaceRole; status: string; version: number } | undefined>;
  updateWorkspace(workspaceId: string, name: string, expectedVersion: number, userId: string): Promise<Workspace | undefined>;
  insertInvitation(id: string, workspaceId: string, tokenHash: string, role: Exclude<WorkspaceRole, "owner">, createdBy: string, expiresAt: Date): Promise<Invitation>;
  getInvitationById(workspaceId: string, invitationId: string, lock?: boolean): Promise<Invitation | undefined>;
  getInvitationByHash(tokenHash: string, lock?: boolean): Promise<(Invitation & { acceptedBy?: string; createdBy: string }) | undefined>;
  revokeInvitation(workspaceId: string, invitationId: string, expectedVersion: number): Promise<Invitation | undefined>;
  acceptInvitation(invitationId: string, userId: string): Promise<boolean>;
  upsertMembership(workspaceId: string, userId: string, role: WorkspaceRole, invitedBy: string): Promise<void>;
  getMembershipView(workspaceId: string, userId: string): Promise<Membership | undefined>;
  updateMembership(workspaceId: string, userId: string, role: WorkspaceRole, expectedVersion: number): Promise<Membership | undefined>;
  removeMembership(workspaceId: string, userId: string, expectedVersion: number): Promise<Membership | undefined>;
  insertAudit(id: string, workspaceId: string, actorUserId: string, type: string, subjectUserId?: string, invitationId?: string, payload?: unknown): Promise<void>;
  listInvitations(workspaceId: string, cursor?: string): Promise<{ items: Invitation[]; nextCursor: string | null }>;
  listMembers(workspaceId: string, cursor?: string): Promise<{ items: Membership[]; nextCursor: string | null }>;
  listEvents(workspaceId: string, afterId?: string): Promise<WorkspaceAuditEvent[]>;
}

export interface WorkspaceStore {
  transaction<T>(action: (transaction: WorkspaceTransaction) => Promise<T>): Promise<T>;
  listWorkspaces(userId: string, cursor?: string): Promise<{ items: Workspace[]; nextCursor: string | null }>;
  getWorkspace(workspaceId: string, userId: string): Promise<Workspace | undefined>;
  listInvitations(workspaceId: string, cursor?: string): Promise<{ items: Invitation[]; nextCursor: string | null }>;
  listMembers(workspaceId: string, cursor?: string): Promise<{ items: Membership[]; nextCursor: string | null }>;
  listEvents(workspaceId: string, afterId?: string): Promise<WorkspaceAuditEvent[]>;
}

export class PgWorkspaceStore implements WorkspaceStore {
  constructor(private readonly database: Database) {}

  async transaction<T>(action: (transaction: WorkspaceTransaction) => Promise<T>): Promise<T> {
    const client = await this.database.connect();
    try {
      await client.query("BEGIN");
      const result = await action(new PgWorkspaceTransaction(client));
      await client.query("COMMIT");
      return result;
    } catch (error) {
      await client.query("ROLLBACK");
      throw error;
    } finally {
      client.release();
    }
  }

  async listWorkspaces(userId: string, cursor = ""): Promise<{ items: Workspace[]; nextCursor: string | null }> {
    const rows = await this.database.query(`SELECT w.*, m.role FROM workspaces w JOIN workspace_memberships m ON m.workspace_id=w.id
      WHERE m.user_id=$1 AND m.status='active' AND w.status='active' AND ($2='' OR w.id::text>$2)
      ORDER BY w.id LIMIT 101`, [userId, cursor]);
    const items = rows.rows.slice(0, 100).map(workspaceRow);
    return { items, nextCursor: rows.rows.length > 100 ? items.at(-1)?.id ?? null : null };
  }

  async getWorkspace(workspaceId: string, userId: string): Promise<Workspace | undefined> {
    const result = await this.database.query(`SELECT w.*, m.role FROM workspaces w JOIN workspace_memberships m ON m.workspace_id=w.id
      WHERE w.id=$1 AND m.user_id=$2 AND m.status='active'`, [workspaceId, userId]);
    return result.rows[0] ? workspaceRow(result.rows[0]) : undefined;
  }

  async listInvitations(workspaceId: string, cursor = ""): Promise<{ items: Invitation[]; nextCursor: string | null }> {
    const result = await this.database.query("SELECT *,expires_at<=now() AS is_expired FROM workspace_invitations WHERE workspace_id=$1 AND ($2='' OR id::text>$2) ORDER BY id LIMIT 101", [workspaceId, cursor]);
    const items = result.rows.slice(0, 100).map(invitationRow);
    return { items, nextCursor: result.rows.length > 100 ? items.at(-1)?.id ?? null : null };
  }

  async listMembers(workspaceId: string, cursor = ""): Promise<{ items: Membership[]; nextCursor: string | null }> {
    const result = await this.database.query(`SELECT m.*,u.email,u.display_name FROM workspace_memberships m JOIN users u ON u.id=m.user_id
      WHERE m.workspace_id=$1 AND ($2='' OR m.user_id::text>$2) ORDER BY m.user_id LIMIT 101`, [workspaceId, cursor]);
    const items = result.rows.slice(0, 100).map(membershipRow);
    return { items, nextCursor: result.rows.length > 100 ? items.at(-1)?.user.id ?? null : null };
  }

  async listEvents(workspaceId: string, afterId = ""): Promise<WorkspaceAuditEvent[]> {
    const result = await this.database.query(`SELECT id,event_type,payload,created_at FROM workspace_audit_events
      WHERE workspace_id=$1 AND ($2='' OR (created_at,id)>(SELECT created_at,id FROM workspace_audit_events WHERE workspace_id=$1 AND id::text=$2))
      ORDER BY created_at,id LIMIT 100`, [workspaceId, afterId]);
    return result.rows.map((row) => ({ id: row.id, type: row.event_type, payload: row.payload, createdAt: row.created_at.toISOString() }));
  }
}

class PgWorkspaceTransaction implements WorkspaceTransaction {
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
  async createWorkspace(id: string, slug: string, name: string, principal: WorkspacePrincipal): Promise<Workspace> {
    const inserted = await this.client.query("INSERT INTO workspaces(id,slug,name,created_by) VALUES($1,$2,$3,$4) RETURNING *", [id, slug, name, principal.userId]);
    await this.client.query("INSERT INTO workspace_memberships(workspace_id,user_id,role) VALUES($1,$2,'owner')", [id, principal.userId]);
    return workspaceRow({ ...inserted.rows[0], role: "owner" });
  }
  async lockWorkspace(workspaceId: string): Promise<{ createdBy: string; version: number } | undefined> {
    const result = await this.client.query("SELECT created_by,version FROM workspaces WHERE id=$1 FOR UPDATE", [workspaceId]);
    return result.rows[0] ? { createdBy: result.rows[0].created_by, version: Number(result.rows[0].version) } : undefined;
  }
  async getWorkspace(workspaceId: string, userId: string): Promise<Workspace | undefined> {
    const result = await this.client.query(`SELECT w.*,m.role FROM workspaces w JOIN workspace_memberships m ON m.workspace_id=w.id WHERE w.id=$1 AND m.user_id=$2 AND m.status='active'`, [workspaceId, userId]);
    return result.rows[0] ? workspaceRow(result.rows[0]) : undefined;
  }
  async getMembership(workspaceId: string, userId: string, lock = false): Promise<{ role: WorkspaceRole; status: string; version: number } | undefined> {
    const result = await this.client.query(`SELECT role,status,version FROM workspace_memberships WHERE workspace_id=$1 AND user_id=$2${lock ? " FOR UPDATE" : ""}`, [workspaceId, userId]);
    return result.rows[0] ? { role: result.rows[0].role, status: result.rows[0].status, version: Number(result.rows[0].version) } : undefined;
  }
  async updateWorkspace(workspaceId: string, name: string, expectedVersion: number, userId: string): Promise<Workspace | undefined> {
    const result = await this.client.query(`UPDATE workspaces SET name=$2,version=version+1,updated_at=now() WHERE id=$1 AND version=$3 RETURNING *`, [workspaceId, name, expectedVersion]);
    if (!result.rows[0]) return undefined;
    const membership = await this.getMembership(workspaceId, userId);
    return workspaceRow({ ...result.rows[0], role: membership?.role });
  }
  async insertInvitation(id: string, workspaceId: string, tokenHash: string, role: Exclude<WorkspaceRole, "owner">, createdBy: string, expiresAt: Date): Promise<Invitation> {
    const result = await this.client.query(`INSERT INTO workspace_invitations(id,workspace_id,token_hash,token_key_id,role,created_by,expires_at)
      VALUES($1,$2,$3,'workspace-invitation-hmac/v1',$4,$5,$6) RETURNING *`, [id, workspaceId, tokenHash, role, createdBy, expiresAt]);
    return invitationRow(result.rows[0]);
  }
  async getInvitationById(workspaceId: string, invitationId: string, lock = false): Promise<Invitation | undefined> {
    const result = await this.client.query(`SELECT *,expires_at<=now() AS is_expired FROM workspace_invitations WHERE workspace_id=$1 AND id=$2${lock ? " FOR UPDATE" : ""}`, [workspaceId, invitationId]);
    return result.rows[0] ? invitationRow(result.rows[0]) : undefined;
  }
  async getInvitationByHash(tokenHash: string, lock = false): Promise<(Invitation & { acceptedBy?: string; createdBy: string }) | undefined> {
    const result = await this.client.query(`SELECT *,expires_at<=now() AS is_expired FROM workspace_invitations WHERE token_hash=$1${lock ? " FOR UPDATE" : ""}`, [tokenHash]);
    return result.rows[0] ? { ...invitationRow(result.rows[0]), createdBy: result.rows[0].created_by, ...(result.rows[0].accepted_by ? { acceptedBy: result.rows[0].accepted_by } : {}) } : undefined;
  }
  async revokeInvitation(workspaceId: string, invitationId: string, expectedVersion: number): Promise<Invitation | undefined> {
    const result = await this.client.query("UPDATE workspace_invitations SET status='revoked',version=version+1 WHERE workspace_id=$1 AND id=$2 AND version=$3 AND status='pending' RETURNING *", [workspaceId, invitationId, expectedVersion]);
    return result.rows[0] ? invitationRow(result.rows[0]) : undefined;
  }
  async acceptInvitation(invitationId: string, userId: string): Promise<boolean> {
    const result = await this.client.query("UPDATE workspace_invitations SET status='accepted',accepted_by=$2,accepted_at=now(),version=version+1 WHERE id=$1 AND status='pending' AND expires_at>now() RETURNING id", [invitationId, userId]);
    return !!result.rowCount;
  }
  async upsertMembership(workspaceId: string, userId: string, role: WorkspaceRole, invitedBy: string): Promise<void> {
    await this.client.query(`INSERT INTO workspace_memberships(workspace_id,user_id,role,invited_by) VALUES($1,$2,$3,$4)
      ON CONFLICT(workspace_id,user_id) DO UPDATE SET role=EXCLUDED.role,status='active',removed_at=NULL,version=workspace_memberships.version+1,invited_by=EXCLUDED.invited_by`, [workspaceId, userId, role, invitedBy]);
  }
  async getMembershipView(workspaceId: string, userId: string): Promise<Membership | undefined> {
    const result = await this.client.query("SELECT m.*,u.email,u.display_name FROM workspace_memberships m JOIN users u ON u.id=m.user_id WHERE m.workspace_id=$1 AND m.user_id=$2", [workspaceId, userId]);
    return result.rows[0] ? membershipRow(result.rows[0]) : undefined;
  }
  async updateMembership(workspaceId: string, userId: string, role: WorkspaceRole, expectedVersion: number): Promise<Membership | undefined> {
    const result = await this.client.query(`UPDATE workspace_memberships SET role=$3,version=version+1 WHERE workspace_id=$1 AND user_id=$2 AND version=$4 AND status='active' RETURNING *`, [workspaceId, userId, role, expectedVersion]);
    if (!result.rows[0]) return undefined;
    return this.getMembershipView(workspaceId, userId);
  }
  async removeMembership(workspaceId: string, userId: string, expectedVersion: number): Promise<Membership | undefined> {
    const result = await this.client.query(`UPDATE workspace_memberships SET status='removed',removed_at=now(),version=version+1 WHERE workspace_id=$1 AND user_id=$2 AND version=$3 AND status='active' RETURNING *`, [workspaceId, userId, expectedVersion]);
    if (!result.rows[0]) return undefined;
    return this.getMembershipView(workspaceId, userId);
  }
  async insertAudit(id: string, workspaceId: string, actorUserId: string, type: string, subjectUserId?: string, invitationId?: string, payload: unknown = {}): Promise<void> {
    await this.client.query("INSERT INTO workspace_audit_events(id,workspace_id,actor_user_id,event_type,subject_user_id,invitation_id,payload) VALUES($1,$2,$3,$4,$5,$6,$7)", [id, workspaceId, actorUserId, type, subjectUserId ?? null, invitationId ?? null, payload]);
  }
  async listInvitations(workspaceId: string, cursor = ""): Promise<{ items: Invitation[]; nextCursor: string | null }> {
    const result = await this.client.query("SELECT *,expires_at<=now() AS is_expired FROM workspace_invitations WHERE workspace_id=$1 AND ($2='' OR id::text>$2) ORDER BY id LIMIT 101", [workspaceId, cursor]);
    const items = result.rows.slice(0, 100).map(invitationRow);
    return { items, nextCursor: result.rows.length > 100 ? items.at(-1)?.id ?? null : null };
  }
  async listMembers(workspaceId: string, cursor = ""): Promise<{ items: Membership[]; nextCursor: string | null }> {
    const result = await this.client.query(`SELECT m.*,u.email,u.display_name FROM workspace_memberships m JOIN users u ON u.id=m.user_id
      WHERE m.workspace_id=$1 AND ($2='' OR m.user_id::text>$2) ORDER BY m.user_id LIMIT 101`, [workspaceId, cursor]);
    const items = result.rows.slice(0, 100).map(membershipRow);
    return { items, nextCursor: result.rows.length > 100 ? items.at(-1)?.user.id ?? null : null };
  }
  async listEvents(workspaceId: string, afterId = ""): Promise<WorkspaceAuditEvent[]> {
    const result = await this.client.query(`SELECT id,event_type,payload,created_at FROM workspace_audit_events
      WHERE workspace_id=$1 AND ($2='' OR (created_at,id)>(SELECT created_at,id FROM workspace_audit_events WHERE workspace_id=$1 AND id::text=$2))
      ORDER BY created_at,id LIMIT 100`, [workspaceId, afterId]);
    return result.rows.map((row) => ({ id: row.id, type: row.event_type, payload: row.payload, createdAt: row.created_at.toISOString() }));
  }
}

function workspaceRow(row: QueryResultRow): Workspace {
  return { id: row.id, slug: row.slug, name: row.name, status: row.status, version: Number(row.version), currentUserRole: row.role, createdAt: row.created_at.toISOString(), updatedAt: row.updated_at.toISOString() };
}
function invitationRow(row: QueryResultRow): Invitation {
  const status = row.status === "pending" && (row.is_expired === true || (row.is_expired === undefined && row.expires_at.getTime() <= Date.now())) ? "expired" : row.status;
  return { id: row.id, workspaceId: row.workspace_id, role: row.role, status, version: Number(row.version), createdAt: row.created_at.toISOString(), expiresAt: row.expires_at.toISOString() };
}
function membershipRow(row: QueryResultRow): Membership {
  return { workspaceId: row.workspace_id, user: { id: row.user_id, email: row.email, displayName: row.display_name, status: "active" }, role: row.role, status: row.status, version: Number(row.version), joinedAt: row.joined_at.toISOString(), ...(row.removed_at ? { removedAt: row.removed_at.toISOString() } : {}) };
}

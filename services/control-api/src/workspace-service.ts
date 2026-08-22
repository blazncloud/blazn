import { randomUUID } from "node:crypto";
import { invitationToken, requestDigest, sha256 } from "./workspace-crypto.js";
import type { IdempotencyReceipt, WorkspaceStore, WorkspaceTransaction } from "./workspace-store.js";
import { roleAllows, type Invitation, type Membership, type MutationResult, type Workspace, WorkspaceHttpError, type WorkspacePrincipal, type WorkspaceRole } from "./workspace-types.js";

interface IdempotentMutation<T> {
  operation: string;
  key: string;
  digest: string;
  targetKey: string;
  replayCapability?: "read" | "edit" | "invite" | "manage_members";
  execute(transaction: WorkspaceTransaction): Promise<{ workspaceId: string; response: T; storedResponse?: unknown; status: number }>;
  replay?(transaction: WorkspaceTransaction, receipt: IdempotencyReceipt): Promise<T>;
}

export class WorkspaceService {
  constructor(private readonly store: WorkspaceStore, private readonly invitationKey: () => Promise<Buffer>) {}

  async createWorkspace(principal: WorkspacePrincipal, idempotencyKey: string, input: { name: string; slug?: string }): Promise<{ workspace: Workspace }> {
    const name = bounded(input.name, "name", 160);
    const slug = normalizeSlug(input.slug ?? name);
    try {
      return await this.idempotent(principal, {
        operation: "workspace.create", key: idempotencyKey, digest: requestDigest({ name, slug }), targetKey: `slug:${slug}`,
        replayCapability: "read",
        execute: async (transaction) => {
          const workspace = await transaction.createWorkspace(randomUUID(), slug, name, principal);
          const response = { workspace };
          await transaction.insertAudit(randomUUID(), workspace.id, principal.userId, "workspace.created", undefined, undefined, { name, slug });
          return { workspaceId: workspace.id, response, status: 201 };
        },
      });
    } catch (error) {
      if (isPgCode(error, "23505")) throw new WorkspaceHttpError("workspace_slug_conflict", "workspace slug is already in use");
      throw error;
    }
  }

  listWorkspaces(principal: WorkspacePrincipal, cursor = "") { return this.store.listWorkspaces(principal.userId, cursor); }

  async getWorkspace(principal: WorkspacePrincipal, workspaceId: string): Promise<{ workspace: Workspace }> {
    const workspace = await this.store.getWorkspace(workspaceId, principal.userId);
    if (!workspace) throw notFound();
    return { workspace };
  }

  async updateWorkspace(principal: WorkspacePrincipal, workspaceId: string, idempotencyKey: string, input: { name: string; expectedVersion: number }): Promise<{ workspace: Workspace }> {
    const name = bounded(input.name, "name", 160);
    positiveVersion(input.expectedVersion);
    return this.idempotent(principal, {
      operation: "workspace.update", key: idempotencyKey, digest: requestDigest({ name, expectedVersion: input.expectedVersion }), targetKey: `workspace:${workspaceId}`,
      replayCapability: "edit",
      execute: async (transaction) => {
        await this.authorize(transaction, principal, workspaceId, "edit", true);
        const workspace = await transaction.updateWorkspace(workspaceId, name, input.expectedVersion, principal.userId);
        if (!workspace) throw new WorkspaceHttpError("version_conflict", "workspace version changed");
        await transaction.insertAudit(randomUUID(), workspaceId, principal.userId, "workspace.updated", undefined, undefined, { name, version: workspace.version });
        return { workspaceId, response: { workspace }, status: 200 };
      },
    });
  }

  async createInvitation(principal: WorkspacePrincipal, workspaceId: string, idempotencyKey: string, input: { role: WorkspaceRole; expiresIn: number }): Promise<{ invitation: Invitation; inviteToken: string }> {
    if (input.role === "owner" || !["administrator", "operator", "member", "viewer"].includes(input.role)) throw new WorkspaceHttpError("invalid_request", "invitation role is invalid");
    if (!Number.isSafeInteger(input.expiresIn) || input.expiresIn < 300 || input.expiresIn > 604800) throw new WorkspaceHttpError("invalid_request", "invitation expiry is invalid");
    const key = await this.invitationKey();
    return this.idempotent(principal, {
      operation: "invitation.create", key: idempotencyKey, digest: requestDigest(input), targetKey: `workspace:${workspaceId}`,
      replayCapability: "invite",
      replay: async (_transaction, receipt) => {
        const body = receipt.responseBody as { invitation: Invitation };
        return { invitation: body.invitation, inviteToken: invitationToken(key, workspaceId, body.invitation.id, idempotencyKey) };
      },
      execute: async (transaction) => {
        await this.authorize(transaction, principal, workspaceId, "invite", true);
        const invitationId = randomUUID();
        const token = invitationToken(key, workspaceId, invitationId, idempotencyKey);
        const invitation = await transaction.insertInvitation(invitationId, workspaceId, sha256(token), input.role, principal.userId, new Date(Date.now() + input.expiresIn * 1000));
        await transaction.insertAudit(randomUUID(), workspaceId, principal.userId, "invitation.created", undefined, invitation.id, { role: input.role, expiresAt: invitation.expiresAt });
        return { workspaceId, response: { invitation, inviteToken: token }, storedResponse: { invitation }, status: 201 };
      },
    });
  }

  async listInvitations(principal: WorkspacePrincipal, workspaceId: string, cursor = "") {
    return this.store.transaction(async (transaction) => {
      await this.authorize(transaction, principal, workspaceId, "invite", true);
      return transaction.listInvitations(workspaceId, cursor);
    });
  }

  async revokeInvitation(principal: WorkspacePrincipal, workspaceId: string, invitationId: string, expectedVersion: number, idempotencyKey: string): Promise<MutationResult> {
    positiveVersion(expectedVersion);
    return this.idempotent(principal, {
      operation: "invitation.revoke", key: idempotencyKey, digest: requestDigest({ expectedVersion }), targetKey: `invitation:${invitationId}`,
      replayCapability: "invite",
      execute: async (transaction) => {
        await this.authorize(transaction, principal, workspaceId, "invite", true);
        const current = await transaction.getInvitationById(workspaceId, invitationId, true);
        if (!current) throw notFound();
        if (current.status === "expired") throw new WorkspaceHttpError("invitation_expired", "invitation is expired");
        if (current.status === "accepted") throw new WorkspaceHttpError("invitation_consumed", "invitation is consumed");
        if (current.status === "revoked") throw new WorkspaceHttpError("invitation_revoked", "invitation is revoked");
        const invitation = await transaction.revokeInvitation(workspaceId, invitationId, expectedVersion);
        if (!invitation) throw new WorkspaceHttpError("version_conflict", "invitation version changed");
        const response: MutationResult = { status: "revoked", workspaceId, invitationId, version: invitation.version };
        await transaction.insertAudit(randomUUID(), workspaceId, principal.userId, "invitation.revoked", undefined, invitationId, { version: invitation.version });
        return { workspaceId, response, status: 200 };
      },
    });
  }

  async acceptInvitation(principal: WorkspacePrincipal, idempotencyKey: string, inviteTokenValue: string): Promise<{ workspace: Workspace }> {
    if (inviteTokenValue.length < 32 || inviteTokenValue.length > 512) throw new WorkspaceHttpError("invitation_invalid", "invitation is invalid");
    const digest = requestDigest({ inviteToken: inviteTokenValue });
    return this.store.transaction(async (transaction) => {
      await transaction.lockIdempotency(principal.userId, "invitation.accept", idempotencyKey);
      const preview = await transaction.getInvitationByHash(sha256(inviteTokenValue), false);
      if (!preview) throw new WorkspaceHttpError("invitation_invalid", "invitation is invalid");
      if (!await transaction.lockWorkspace(preview.workspaceId)) throw new WorkspaceHttpError("invitation_invalid", "invitation is invalid");
      const invitation = await transaction.getInvitationByHash(sha256(inviteTokenValue), true);
      if (!invitation || invitation.id !== preview.id) throw new WorkspaceHttpError("invitation_invalid", "invitation is invalid");
      const receipt = await transaction.getIdempotency(principal.userId, "invitation.accept", idempotencyKey);
      if (receipt) {
        this.verifyReceipt(receipt, invitation.workspaceId, `invitation:${invitation.id}`, digest);
        await this.authorize(transaction, principal, invitation.workspaceId, "read", false);
        return receipt.responseBody as { workspace: Workspace };
      }
      if (invitation.status === "expired") throw new WorkspaceHttpError("invitation_expired", "invitation is expired");
      if (invitation.status === "revoked") throw new WorkspaceHttpError("invitation_revoked", "invitation is revoked");
      if (invitation.status === "accepted") throw new WorkspaceHttpError("invitation_consumed", "invitation is consumed");
      await transaction.upsertMembership(invitation.workspaceId, principal.userId, invitation.role, invitation.createdBy);
      await transaction.acceptInvitation(invitation.id, principal.userId);
      const workspace = await transaction.getWorkspace(invitation.workspaceId, principal.userId);
      if (!workspace) throw new Error("workspace membership was not created");
      const response = { workspace };
      await transaction.insertAudit(randomUUID(), invitation.workspaceId, principal.userId, "invitation.accepted", principal.userId, invitation.id, { role: invitation.role });
      await transaction.putIdempotency(principal.userId, "invitation.accept", idempotencyKey, { workspaceId: invitation.workspaceId, targetKey: `invitation:${invitation.id}`, requestDigest: digest, responseStatus: 200, responseBody: response });
      return response;
    });
  }

  async listMembers(principal: WorkspacePrincipal, workspaceId: string, cursor = "") {
    return this.store.transaction(async (transaction) => {
      await this.authorize(transaction, principal, workspaceId, "read", true);
      return transaction.listMembers(workspaceId, cursor);
    });
  }

  async updateMember(principal: WorkspacePrincipal, workspaceId: string, userId: string, idempotencyKey: string, input: { role: WorkspaceRole; expectedVersion: number }): Promise<Membership> {
    if (input.role === "owner" || !["administrator", "operator", "member", "viewer"].includes(input.role)) throw new WorkspaceHttpError("invalid_request", "membership role is invalid");
    positiveVersion(input.expectedVersion);
    return this.idempotent(principal, {
      operation: "membership.update", key: idempotencyKey, digest: requestDigest(input), targetKey: `user:${userId}`, replayCapability: "manage_members",
      execute: async (transaction) => {
        const workspace = await this.authorize(transaction, principal, workspaceId, "manage_members", true);
        const target = await transaction.getMembership(workspaceId, userId, true);
        if (!target || target.status !== "active") throw notFound();
        if (workspace.createdBy === userId || target.role === "owner") throw new WorkspaceHttpError("last_owner", "initial owner role is immutable");
        const membership = await transaction.updateMembership(workspaceId, userId, input.role, input.expectedVersion);
        if (!membership) throw new WorkspaceHttpError("version_conflict", "membership version changed");
        await transaction.insertAudit(randomUUID(), workspaceId, principal.userId, "membership.role_changed", userId, undefined, { role: input.role, version: membership.version });
        return { workspaceId, response: membership, status: 200 };
      },
    });
  }

  async removeMember(principal: WorkspacePrincipal, workspaceId: string, userId: string, expectedVersion: number, idempotencyKey: string): Promise<MutationResult> {
    return this.removeMembership(principal, workspaceId, userId, expectedVersion, idempotencyKey, false);
  }
  async leaveWorkspace(principal: WorkspacePrincipal, workspaceId: string, expectedVersion: number, idempotencyKey: string): Promise<MutationResult> {
    return this.removeMembership(principal, workspaceId, principal.userId, expectedVersion, idempotencyKey, true);
  }

  async eventBatch(principal: WorkspacePrincipal, workspaceId: string, afterId = "") {
    return this.store.transaction(async (transaction) => {
      await this.authorize(transaction, principal, workspaceId, "read", false);
      return transaction.listEvents(workspaceId, afterId);
    });
  }

  private async removeMembership(principal: WorkspacePrincipal, workspaceId: string, userId: string, expectedVersion: number, idempotencyKey: string, leave: boolean): Promise<MutationResult> {
    positiveVersion(expectedVersion);
    return this.idempotent(principal, {
      operation: leave ? "membership.leave" : "membership.remove", key: idempotencyKey, digest: requestDigest({ userId, expectedVersion }), targetKey: `user:${userId}`,
      replayCapability: leave ? undefined : "manage_members",
      execute: async (transaction) => {
        const workspace = await this.authorize(transaction, principal, workspaceId, leave ? "read" : "manage_members", true);
        const target = await transaction.getMembership(workspaceId, userId, true);
        if (!target || target.status !== "active") throw notFound();
        if (workspace.createdBy === userId || target.role === "owner") throw new WorkspaceHttpError("last_owner", "initial owner cannot leave or be removed");
        const membership = await transaction.removeMembership(workspaceId, userId, expectedVersion);
        if (!membership) throw new WorkspaceHttpError("version_conflict", "membership version changed");
        const response: MutationResult = { status: leave ? "left" : "removed", workspaceId, userId, version: membership.version };
        await transaction.insertAudit(randomUUID(), workspaceId, principal.userId, leave ? "membership.left" : "membership.removed", userId, undefined, { version: membership.version });
        return { workspaceId, response, status: 200 };
      },
    });
  }

  private async authorize(transaction: WorkspaceTransaction, principal: WorkspacePrincipal, workspaceId: string, capability: "read" | "edit" | "invite" | "manage_members", lockWorkspace: boolean) {
    const locked = lockWorkspace ? await transaction.lockWorkspace(workspaceId) : undefined;
    const workspace = await transaction.getWorkspace(workspaceId, principal.userId);
    if (!workspace || (lockWorkspace && !locked)) throw notFound();
    if (!roleAllows(workspace.currentUserRole, capability)) throw new WorkspaceHttpError("permission_denied", "workspace action is not permitted");
    return { ...workspace, createdBy: locked?.createdBy ?? "" };
  }

  private async idempotent<T>(principal: WorkspacePrincipal, mutation: IdempotentMutation<T>): Promise<T> {
    validIdempotencyKey(mutation.key);
    return this.store.transaction(async (transaction) => {
      await transaction.lockIdempotency(principal.userId, mutation.operation, mutation.key);
      const receipt = await transaction.getIdempotency(principal.userId, mutation.operation, mutation.key);
      if (receipt) {
        this.verifyReceipt(receipt, receipt.workspaceId, mutation.targetKey, mutation.digest);
        await this.authorize(transaction, principal, receipt.workspaceId, mutation.replayCapability ?? "read", false);
        return mutation.replay ? mutation.replay(transaction, receipt) : receipt.responseBody as T;
      }
      const result = await mutation.execute(transaction);
      await transaction.putIdempotency(principal.userId, mutation.operation, mutation.key, { workspaceId: result.workspaceId, targetKey: mutation.targetKey, requestDigest: mutation.digest, responseStatus: result.status, responseBody: result.storedResponse ?? result.response });
      return result.response;
    });
  }

  private verifyReceipt(receipt: IdempotencyReceipt, workspaceId: string, targetKey: string, digest: string): void {
    if (receipt.workspaceId !== workspaceId || receipt.targetKey !== targetKey || receipt.requestDigest !== digest) throw new WorkspaceHttpError("idempotency_conflict", "idempotency key is bound to another request");
  }
}

function normalizeSlug(value: string): string {
  const slug = value.trim().toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
  if (!/^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$/.test(slug)) throw new WorkspaceHttpError("invalid_request", "workspace slug must contain 3 to 64 lowercase letters, digits, or hyphens");
  return slug;
}
function bounded(value: string, field: string, max: number): string {
  const trimmed = value.trim();
  if (!trimmed || trimmed.length > max) throw new WorkspaceHttpError("invalid_request", `${field} is invalid`);
  return trimmed;
}
function positiveVersion(value: number): void {
  if (!Number.isSafeInteger(value) || value < 1) throw new WorkspaceHttpError("invalid_request", "expectedVersion must be a positive integer");
}
function validIdempotencyKey(value: string): void {
  if (value.length < 8 || value.length > 128) throw new WorkspaceHttpError("invalid_request", "Idempotency-Key is invalid");
}
function notFound(): WorkspaceHttpError { return new WorkspaceHttpError("workspace_not_found", "workspace was not found"); }
function isPgCode(error: unknown, code: string): boolean { return !!error && typeof error === "object" && "code" in error && error.code === code; }

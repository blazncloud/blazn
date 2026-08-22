import { randomUUID } from "node:crypto";
import { requestDigest } from "./workspace-crypto.js";
import type { IdempotencyReceipt } from "./workspace-store.js";
import { roleAllows } from "./workspace-types.js";
import type { ProjectStore, ProjectTransaction } from "./project-store.js";
import { ProjectHttpError, type Project, type ProjectPrincipal, type ProjectStatus } from "./project-types.js";

export interface CreateProjectInput {
  name: string;
  slug?: string;
  kind?: string;
  description?: string;
}

export interface UpdateProjectInput {
  expectedVersion: number;
  name?: string;
  description?: string;
  status?: ProjectStatus;
}

export class ProjectService {
  constructor(private readonly store: ProjectStore) {}

  async createProject(principal: ProjectPrincipal, workspaceId: string, idempotencyKey: string, input: CreateProjectInput): Promise<{ project: Project }> {
    const name = bounded(input.name, "name", 160);
    const slug = normalizeSlug(input.slug ?? name);
    const kind = normalizeKind(input.kind ?? "general");
    const description = descriptionValue(input.description ?? "");
    const digest = requestDigest({ workspaceId, slug, kind, name, description });
    try {
      return await this.idempotent(principal, workspaceId, "project.create", idempotencyKey, `slug:${slug}`, digest, "edit", async (transaction) => {
        const project = await transaction.createProject({
          id: randomUUID(), workspaceId, slug, kind, name, description,
          status: "active", version: 1, createdBy: principal.userId,
        });
        await transaction.insertAudit(randomUUID(), workspaceId, principal.userId, "project.created", { projectId: project.id, slug, kind, version: project.version });
        return { project };
      }, 201);
    } catch (error) {
      if (isPgCode(error, "23505")) throw new ProjectHttpError("project_slug_conflict", "project slug is already in use in this Workspace");
      throw error;
    }
  }

  async listProjects(principal: ProjectPrincipal, workspaceId: string, status: ProjectStatus | "all" = "active", cursor = ""): Promise<{ items: Project[]; nextCursor: string | null }> {
    if (!(["active", "archived", "all"] as const).includes(status)) throw new ProjectHttpError("invalid_request", "project status filter is invalid");
    if (cursor.length > 512) throw new ProjectHttpError("invalid_request", "project cursor is invalid");
    return this.store.transaction(async (transaction) => {
      await this.authorize(transaction, principal, workspaceId, "read", true);
      return transaction.listProjects(workspaceId, status, cursor);
    });
  }

  async getProject(principal: ProjectPrincipal, workspaceId: string, projectId: string): Promise<{ project: Project }> {
    return this.store.transaction(async (transaction) => {
      await this.authorize(transaction, principal, workspaceId, "read", true);
      const project = await transaction.getProject(workspaceId, projectId);
      if (!project) throw new ProjectHttpError("project_not_found", "project was not found");
      return { project };
    });
  }

  async updateProject(principal: ProjectPrincipal, workspaceId: string, projectId: string, idempotencyKey: string, input: UpdateProjectInput): Promise<{ project: Project }> {
    positiveVersion(input.expectedVersion);
    const changes: { name?: string; description?: string; status?: ProjectStatus } = {};
    if (input.name !== undefined) changes.name = bounded(input.name, "name", 160);
    if (input.description !== undefined) changes.description = descriptionValue(input.description);
    if (input.status !== undefined) {
      if (input.status !== "active" && input.status !== "archived") throw new ProjectHttpError("invalid_request", "project status is invalid");
      changes.status = input.status;
    }
    if (Object.keys(changes).length === 0) throw new ProjectHttpError("invalid_request", "project update must change name, description, or status");
    const digest = requestDigest({ workspaceId, projectId, expectedVersion: input.expectedVersion, ...changes });
    return this.idempotent(principal, workspaceId, "project.update", idempotencyKey, `project:${projectId}`, digest, "edit", async (transaction) => {
      const current = await transaction.getProject(workspaceId, projectId, true);
      if (!current) throw new ProjectHttpError("project_not_found", "project was not found");
      if (current.version !== input.expectedVersion) throw new ProjectHttpError("version_conflict", "project version changed");
      const project = await transaction.updateProject(workspaceId, projectId, input.expectedVersion, changes);
      if (!project) throw new ProjectHttpError("version_conflict", "project version changed");
      await transaction.insertAudit(randomUUID(), workspaceId, principal.userId, "project.updated", { projectId, status: project.status, version: project.version });
      return { project };
    }, 200);
  }

  private async authorize(transaction: ProjectTransaction, principal: ProjectPrincipal, workspaceId: string, capability: "read" | "edit", lock: boolean): Promise<void> {
    const access = await transaction.getWorkspaceAccess(workspaceId, principal.userId, lock);
    if (!access || access.workspaceStatus !== "active") throw new ProjectHttpError("workspace_not_found", "workspace was not found");
    if (!roleAllows(access.role, capability)) throw new ProjectHttpError("permission_denied", "project action is not permitted");
  }

  private async idempotent<T>(principal: ProjectPrincipal, workspaceId: string, operation: string, key: string, targetKey: string, digest: string, capability: "read" | "edit", execute: (transaction: ProjectTransaction) => Promise<T>, responseStatus: number): Promise<T> {
    validIdempotencyKey(key);
    return this.store.transaction(async (transaction) => {
      await transaction.lockIdempotency(principal.userId, operation, key);
      const receipt = await transaction.getIdempotency(principal.userId, operation, key);
      if (receipt) {
        this.verifyReceipt(receipt, workspaceId, targetKey, digest);
        await this.authorize(transaction, principal, workspaceId, capability, true);
        return receipt.responseBody as T;
      }
      await this.authorize(transaction, principal, workspaceId, capability, true);
      const response = await execute(transaction);
      await transaction.putIdempotency(principal.userId, operation, key, { workspaceId, targetKey, requestDigest: digest, responseStatus, responseBody: response });
      return response;
    });
  }

  private verifyReceipt(receipt: IdempotencyReceipt, workspaceId: string, targetKey: string, digest: string): void {
    if (receipt.workspaceId !== workspaceId || receipt.targetKey !== targetKey || receipt.requestDigest !== digest) throw new ProjectHttpError("idempotency_conflict", "idempotency key is bound to another request");
  }
}

function normalizeSlug(value: string): string {
  const slug = value.trim().toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
  if (!/^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$/.test(slug)) throw new ProjectHttpError("invalid_request", "project slug must contain 3 to 64 lowercase letters, digits, or hyphens");
  return slug;
}

function normalizeKind(value: string): string {
  const kind = value.trim().toLowerCase();
  if (!/^[a-z][a-z0-9-]{0,62}$/.test(kind)) throw new ProjectHttpError("invalid_request", "project kind is invalid");
  return kind;
}

function bounded(value: string, field: string, max: number): string {
  const trimmed = value.trim();
  if (!trimmed || trimmed.length > max) throw new ProjectHttpError("invalid_request", `${field} is invalid`);
  return trimmed;
}

function descriptionValue(value: string): string {
  const trimmed = value.trim();
  if (trimmed.length > 4000) throw new ProjectHttpError("invalid_request", "description is invalid");
  return trimmed;
}

function positiveVersion(value: number): void {
  if (!Number.isSafeInteger(value) || value < 1) throw new ProjectHttpError("invalid_request", "expectedVersion must be a positive integer");
}

function validIdempotencyKey(value: string): void {
  if (value.length < 8 || value.length > 128) throw new ProjectHttpError("invalid_request", "Idempotency-Key is invalid");
}

function isPgCode(error: unknown, code: string): boolean {
  return !!error && typeof error === "object" && "code" in error && error.code === code;
}

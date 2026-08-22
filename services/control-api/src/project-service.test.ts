import assert from "node:assert/strict";
import test from "node:test";
import { ProjectService } from "./project-service.js";
import type { ProjectStore, ProjectTransaction } from "./project-store.js";
import { ProjectHttpError, type Project, type ProjectAccess, type ProjectPrincipal, type ProjectStatus } from "./project-types.js";
import type { IdempotencyReceipt } from "./workspace-store.js";

const owner: ProjectPrincipal = { userId: "00000000-0000-4000-8000-000000000003", email: "owner@example.test", displayName: "Owner" };
const viewer: ProjectPrincipal = { userId: "00000000-0000-4000-8000-000000000004", email: "viewer@example.test", displayName: "Viewer" };
const workspaceId = "00000000-0000-4000-8000-000000000001";

class MemoryProjectStore implements ProjectStore, ProjectTransaction {
  readonly projects = new Map<string, Project>();
  readonly receipts = new Map<string, IdempotencyReceipt>();
  readonly audits: Array<{ type: string; payload: unknown }> = [];
  readonly access = new Map<string, ProjectAccess>([
    [`${workspaceId}:${owner.userId}`, { workspaceStatus: "active", role: "owner" }],
    [`${workspaceId}:${viewer.userId}`, { workspaceStatus: "active", role: "viewer" }],
  ]);

  async transaction<T>(action: (transaction: ProjectTransaction) => Promise<T>): Promise<T> { return action(this); }
  async lockIdempotency(): Promise<void> {}
  async getIdempotency(principalId: string, operation: string, key: string): Promise<IdempotencyReceipt | undefined> { return this.receipts.get(`${principalId}:${operation}:${key}`); }
  async putIdempotency(principalId: string, operation: string, key: string, receipt: IdempotencyReceipt): Promise<void> { this.receipts.set(`${principalId}:${operation}:${key}`, structuredClone(receipt)); }
  async getWorkspaceAccess(workspace: string, user: string): Promise<ProjectAccess | undefined> { return this.access.get(`${workspace}:${user}`); }
  async createProject(project: Omit<Project, "createdAt" | "updatedAt">): Promise<Project> {
    if ([...this.projects.values()].some((value) => value.workspaceId === project.workspaceId && value.slug === project.slug)) throw Object.assign(new Error("unique"), { code: "23505" });
    const now = "2026-08-22T00:00:00.000Z";
    const stored = { ...project, createdAt: now, updatedAt: now };
    this.projects.set(project.id, stored);
    return structuredClone(stored);
  }
  async getProject(workspace: string, projectId: string): Promise<Project | undefined> {
    const project = this.projects.get(projectId);
    return project?.workspaceId === workspace ? structuredClone(project) : undefined;
  }
  async listProjects(workspace: string, status: ProjectStatus | "all", cursor = ""): Promise<{ items: Project[]; nextCursor: string | null }> {
    const items = [...this.projects.values()].filter((project) => project.workspaceId === workspace && (status === "all" || project.status === status) && project.id > cursor).sort((a, b) => a.id.localeCompare(b.id));
    return { items: structuredClone(items), nextCursor: null };
  }
  async updateProject(workspace: string, projectId: string, expectedVersion: number, changes: { name?: string; description?: string; status?: ProjectStatus }): Promise<Project | undefined> {
    const current = this.projects.get(projectId);
    if (!current || current.workspaceId !== workspace || current.version !== expectedVersion) return undefined;
    const updated = { ...current, ...changes, version: current.version + 1, updatedAt: "2026-08-22T00:01:00.000Z" };
    this.projects.set(projectId, updated);
    return structuredClone(updated);
  }
  async insertAudit(_id: string, _workspace: string, _actor: string, type: string, payload: unknown): Promise<void> { this.audits.push({ type, payload }); }
}

test("Project create is idempotent, tenant-bound, and auditable", async () => {
  const store = new MemoryProjectStore();
  const service = new ProjectService(store);
  const first = await service.createProject(owner, workspaceId, "project-create-1", { name: "Launch Video", kind: "content" });
  const second = await service.createProject(owner, workspaceId, "project-create-1", { name: "Launch Video", kind: "content" });
  assert.equal(first.project.id, second.project.id);
  assert.equal(first.project.slug, "launch-video");
  assert.equal(first.project.workspaceId, workspaceId);
  assert.equal(first.project.createdBy, owner.userId);
  assert.equal(store.projects.size, 1);
  assert.deepEqual(store.audits.map((event) => event.type), ["project.created"]);
  await assert.rejects(() => service.createProject(owner, workspaceId, "project-create-1", { name: "Different" }), (error: unknown) => error instanceof ProjectHttpError && error.code === "idempotency_conflict");
});

test("Project permissions allow reads but deny viewer mutation", async () => {
  const store = new MemoryProjectStore();
  const service = new ProjectService(store);
  const created = await service.createProject(owner, workspaceId, "project-create-2", { name: "Campaign" });
  assert.equal((await service.listProjects(viewer, workspaceId)).items.length, 1);
  assert.equal((await service.getProject(viewer, workspaceId, created.project.id)).project.id, created.project.id);
  await assert.rejects(() => service.createProject(viewer, workspaceId, "project-create-3", { name: "Denied" }), (error: unknown) => error instanceof ProjectHttpError && error.code === "permission_denied");
  await assert.rejects(() => service.getProject(owner, "00000000-0000-4000-8000-000000000099", created.project.id), (error: unknown) => error instanceof ProjectHttpError && error.code === "workspace_not_found");
});

test("Project update uses optimistic versions and archive lifecycle", async () => {
  const store = new MemoryProjectStore();
  const service = new ProjectService(store);
  const created = await service.createProject(owner, workspaceId, "project-create-4", { name: "Campaign" });
  const archived = await service.updateProject(owner, workspaceId, created.project.id, "project-update-1", { expectedVersion: 1, description: "Complete", status: "archived" });
  assert.equal(archived.project.version, 2);
  assert.equal(archived.project.status, "archived");
  assert.equal((await service.listProjects(owner, workspaceId, "active")).items.length, 0);
  assert.equal((await service.listProjects(owner, workspaceId, "archived")).items.length, 1);
  const replay = await service.updateProject(owner, workspaceId, created.project.id, "project-update-1", { expectedVersion: 1, description: "Complete", status: "archived" });
  assert.equal(replay.project.version, 2);
  await assert.rejects(() => service.updateProject(owner, workspaceId, created.project.id, "project-update-2", { expectedVersion: 1, name: "Stale" }), (error: unknown) => error instanceof ProjectHttpError && error.code === "version_conflict");
  assert.deepEqual(store.audits.map((event) => event.type), ["project.created", "project.updated"]);
});

test("Project slug uniqueness is scoped to the Workspace", async () => {
  const store = new MemoryProjectStore();
  const service = new ProjectService(store);
  await service.createProject(owner, workspaceId, "project-create-5", { name: "Same Name" });
  await assert.rejects(() => service.createProject(owner, workspaceId, "project-create-6", { name: "Same Name" }), (error: unknown) => error instanceof ProjectHttpError && error.code === "project_slug_conflict");
});

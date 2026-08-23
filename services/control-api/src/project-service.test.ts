import assert from "node:assert/strict";
import test from "node:test";
import { ProjectService } from "./project-service.js";
import type { ProjectStore, ProjectTransaction } from "./project-store.js";
import { ProjectHttpError, type Project, type ProjectAccess, type ProjectPrincipal, type ProjectProfile, type ProjectProfileStatus, type ProjectStatus } from "./project-types.js";
import type { IdempotencyReceipt } from "./workspace-store.js";

const owner: ProjectPrincipal = { userId: "00000000-0000-4000-8000-000000000003", email: "owner@example.test", displayName: "Owner" };
const viewer: ProjectPrincipal = { userId: "00000000-0000-4000-8000-000000000004", email: "viewer@example.test", displayName: "Viewer" };
const workspaceId = "00000000-0000-4000-8000-000000000001";
const profileArtifactId="00000000-0000-4000-8000-000000000005",profileDigest=`sha256:${"a".repeat(64)}`;

class MemoryProjectStore implements ProjectStore, ProjectTransaction {
  readonly projects = new Map<string, Project>();
  readonly receipts = new Map<string, IdempotencyReceipt>();
  readonly profiles=new Map<string,ProjectProfile>();
  readonly artifacts=new Map<string,{workspaceId:string;projectId:string;digest:string;status:string}>();
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
  async getProjectProfile(workspace:string,projectId:string,kind:string){const value=this.profiles.get(`${workspace}:${projectId}:${kind}`);return value?structuredClone(value):undefined;}
  async getProfileArtifact(workspace:string,projectId:string,artifactId:string){const value=this.artifacts.get(artifactId);return value?.workspaceId===workspace&&value.projectId===projectId?{digest:value.digest,status:value.status}:undefined;}
  async putProjectProfile(input:{workspaceId:string;projectId:string;kind:string;schemaVersion:string;draftId:string;artifactId:string;digest:string;status:ProjectProfileStatus;expectedVersion:number;userId:string}){const key=`${input.workspaceId}:${input.projectId}:${input.kind}`,current=this.profiles.get(key);if(input.expectedVersion===0&&current||input.expectedVersion>0&&(!current||current.version!==input.expectedVersion))return undefined;const now="2026-08-23T00:00:00.000Z",profile:ProjectProfile={workspaceId:input.workspaceId,projectId:input.projectId,kind:input.kind,schemaVersion:input.schemaVersion,version:(current?.version??0)+1,draftId:input.draftId,artifactId:input.artifactId,digest:input.digest,status:input.status,createdBy:current?.createdBy??input.userId,updatedBy:input.userId,createdAt:current?.createdAt??now,updatedAt:now};this.profiles.set(key,profile);return structuredClone(profile);}
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

test("Project profiles bind ready Artifacts with optimistic idempotent versions",async()=>{const store=new MemoryProjectStore(),service=new ProjectService(store),created=await service.createProject(owner,workspaceId,"project-profile-create",{name:"Content",kind:"content"});store.artifacts.set(profileArtifactId,{workspaceId,projectId:created.project.id,digest:profileDigest,status:"ready"});const input={schemaVersion:"blazn.content/project/v1alpha1",draftId:"00000000-0000-4000-8000-000000000004",artifactId:profileArtifactId,digest:profileDigest,status:"active" as const,expectedVersion:0};const first=await service.putProjectProfile(owner,workspaceId,created.project.id,"content","profile-put-1",input),replay=await service.putProjectProfile(owner,workspaceId,created.project.id,"content","profile-put-1",input);assert.equal(first.profile.version,1);assert.equal(replay.profile.artifactId,profileArtifactId);assert.equal((await service.getProjectProfile(viewer,workspaceId,created.project.id,"content")).profile.draftId,input.draftId);const second=await service.putProjectProfile(owner,workspaceId,created.project.id,"content","profile-put-2",{...input,expectedVersion:1,status:"archived"});assert.equal(second.profile.version,2);assert.equal(second.profile.status,"archived");await assert.rejects(()=>service.putProjectProfile(owner,workspaceId,created.project.id,"content","profile-put-stale",{...input,expectedVersion:1}),isCode("version_conflict"));await assert.rejects(()=>service.putProjectProfile(viewer,workspaceId,created.project.id,"content","profile-put-viewer",{...input,expectedVersion:2}),isCode("permission_denied"));store.artifacts.get(profileArtifactId)!.status="deleted";await assert.rejects(()=>service.putProjectProfile(owner,workspaceId,created.project.id,"content","profile-put-deleted",{...input,expectedVersion:2}),isCode("artifact_not_found"));});

function isCode(code:string){return(error:unknown)=>error instanceof ProjectHttpError&&error.code===code;}

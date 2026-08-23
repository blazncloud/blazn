import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import test from "node:test";
import { createDatabase } from "./db.js";
import { ProjectService } from "./project-service.js";
import { PgProjectStore } from "./project-store.js";
import { ProjectHttpError, type ProjectPrincipal } from "./project-types.js";

const runtimeUrl = process.env.BLAZN_PROJECT_TEST_DATABASE_URL;
const adminUrl = process.env.BLAZN_PROJECT_TEST_ADMIN_DATABASE_URL;

test("PostgreSQL Project idempotency, optimistic updates, tenant isolation, and no-delete role", { skip: !runtimeUrl || !adminUrl }, async () => {
  const runtime = createDatabase(runtimeUrl!); const admin = createDatabase(adminUrl!);
  const owner = principal(); const viewer = principal(); const outsider = principal();
  const workspaceId = randomUUID(); const otherWorkspaceId = randomUUID();
  try {
    for (const user of [owner, viewer, outsider]) await admin.query("INSERT INTO users(id,email,display_name,password_salt,password_hash) VALUES($1,$2,$3,'salt','hash')", [user.userId, user.email, user.displayName]);
    await admin.query("INSERT INTO workspaces(id,slug,name,created_by) VALUES($1,$2,'Project Test',$3),($4,$5,'Other Project Test',$6)", [workspaceId, `project-${owner.userId.slice(0, 8)}`, owner.userId, otherWorkspaceId, `other-${outsider.userId.slice(0, 8)}`, outsider.userId]);
    await admin.query("INSERT INTO workspace_memberships(workspace_id,user_id,role) VALUES($1,$2,'owner'),($1,$3,'viewer'),($4,$5,'owner')", [workspaceId, owner.userId, viewer.userId, otherWorkspaceId, outsider.userId]);
    const service = new ProjectService(new PgProjectStore(runtime));
    const [first, replay] = await Promise.all([
      service.createProject(owner, workspaceId, "project-create-pg-1", { name: "Launch Video", kind: "content" }),
      service.createProject(owner, workspaceId, "project-create-pg-1", { name: "Launch Video", kind: "content" }),
    ]);
    assert.equal(first.project.id, replay.project.id);
    const profileArtifactId=randomUUID(),profileDraftId=randomUUID(),profileDigest=`sha256:${"a".repeat(64)}`;await admin.query("INSERT INTO artifacts(id,workspace_id,project_id,kind,media_type,name,status,digest,size_bytes,object_key,created_by) VALUES($1,$2,$3,'content.manifest','data','content-project.json','ready',$4,12,$5,$6)",[profileArtifactId,workspaceId,first.project.id,profileDigest,`profiles/${profileArtifactId}`,owner.userId]);const profileInput={schemaVersion:"blazn.content/project/v1alpha1",draftId:profileDraftId,artifactId:profileArtifactId,digest:profileDigest,status:"active" as const,expectedVersion:0};const profile=await service.putProjectProfile(owner,workspaceId,first.project.id,"content","profile-put-pg-1",profileInput);assert.equal(profile.profile.version,1);assert.equal((await service.getProjectProfile(viewer,workspaceId,first.project.id,"content")).profile.artifactId,profileArtifactId);const profileReplay=await service.putProjectProfile(owner,workspaceId,first.project.id,"content","profile-put-pg-1",profileInput);assert.equal(profileReplay.profile.version,1);await assert.rejects(()=>service.putProjectProfile(viewer,workspaceId,first.project.id,"content","profile-viewer-pg",{...profileInput,expectedVersion:1}),isCode("permission_denied"));await assert.rejects(()=>service.putProjectProfile(owner,workspaceId,first.project.id,"content","profile-digest-pg",{...profileInput,expectedVersion:1,digest:`sha256:${"b".repeat(64)}`}),isCode("artifact_not_found"));await assert.rejects(()=>admin.query("UPDATE artifacts SET status='deleted',object_key=NULL WHERE id=$1",[profileArtifactId]),pgCode("23514"));
    await assert.rejects(() => service.createProject(owner, workspaceId, "project-create-pg-1", { name: "Changed" }), isCode("idempotency_conflict"));
    assert.equal((await service.listProjects(viewer, workspaceId)).items.length, 1);
    assert.equal((await service.getProject(viewer, workspaceId, first.project.id)).project.id, first.project.id);
    await assert.rejects(() => service.updateProject(viewer, workspaceId, first.project.id, "project-viewer-denied", { expectedVersion: 1, status: "archived" }), isCode("permission_denied"));
    await assert.rejects(() => service.getProject(owner, otherWorkspaceId, first.project.id), isCode("workspace_not_found"));

    const updates = await Promise.allSettled([
      service.updateProject(owner, workspaceId, first.project.id, "project-update-pg-1", { expectedVersion: 1, description: "Complete" }),
      service.updateProject(owner, workspaceId, first.project.id, "project-update-pg-2", { expectedVersion: 1, status: "archived" }),
    ]);
    assert.equal(updates.filter((result) => result.status === "fulfilled").length, 1);
    assert.equal(updates.filter((result) => result.status === "rejected" && result.reason instanceof ProjectHttpError && result.reason.code === "version_conflict").length, 1);
    const current = (await service.getProject(owner, workspaceId, first.project.id)).project;
    assert.equal(current.version, 2);
    const archived = await service.updateProject(owner, workspaceId, first.project.id, "project-archive-pg-1", { expectedVersion: 2, status: "archived" });
    assert.equal(archived.project.status, "archived");
    assert.equal((await service.listProjects(owner, workspaceId, "active")).items.length, 0);
    assert.equal((await service.listProjects(owner, workspaceId, "archived")).items.length, 1);
    const audits = await admin.query<{ event_type: string }>("SELECT event_type FROM workspace_audit_events WHERE workspace_id=$1 AND event_type LIKE 'project.%' ORDER BY created_at,id", [workspaceId]);
    assert.deepEqual(audits.rows.map((row) => row.event_type), ["project.created", "project.profile_put", "project.updated", "project.updated"]);
    await assert.rejects(() => runtime.query("DELETE FROM projects WHERE id=$1", [first.project.id]), (error: unknown) => !!error && typeof error === "object" && "code" in error && error.code === "42501");
  } finally {
    await admin.query("DELETE FROM workspaces WHERE id=ANY($1::uuid[])", [[workspaceId, otherWorkspaceId]]).catch(() => {});
    await admin.query("DELETE FROM users WHERE id=ANY($1::uuid[])", [[owner.userId, viewer.userId, outsider.userId]]).catch(() => {});
    await runtime.end(); await admin.end();
  }
});

function principal(): ProjectPrincipal {
  const userId = randomUUID();
  return { userId, email: `${userId}@example.test`, displayName: userId.slice(0, 8) };
}

function isCode(code: string): (error: unknown) => boolean {
  return (error) => error instanceof ProjectHttpError && error.code === code;
}
function pgCode(code:string){return(error:unknown)=>!!error&&typeof error==="object"&&"code" in error&&error.code===code;}

import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import pg from "pg";
import { SandboxService } from "./sandbox-service.js";
import { PgSandboxStore } from "./sandbox-store.js";
import { SandboxHttpError,type SandboxPrincipal,type SandboxTemplateManifest } from "./sandbox-types.js";

const runtimeUrl=process.env.BLAZN_SANDBOX_API_TEST_DATABASE_URL,adminUrl=process.env.BLAZN_SANDBOX_API_TEST_ADMIN_DATABASE_URL;
test("restricted runtime implements template publish, bound create, hiding, idempotency, operations, and grants",{skip:!runtimeUrl||!adminUrl},async()=>{const runtime=new pg.Pool({connectionString:runtimeUrl}),admin=new pg.Pool({connectionString:adminUrl});const owner=principal("1"),other=principal("2");try{await admin.query("TRUNCATE sandbox_audit_events,sandbox_idempotency_receipts,sandbox_artifacts,sandbox_access_grants,sandbox_events,sandbox_operation_terminal_receipts,sandbox_operations,sandboxes,sandbox_template_version_status,sandbox_template_versions,sandbox_templates,workspace_memberships,workspaces,sessions,devices,users CASCADE");for(const p of [owner,other]){await admin.query("INSERT INTO users(id,email,display_name,password_salt,password_hash) VALUES($1,$2,$3,'salt','hash')",[p.userId,p.email,p.displayName]);await admin.query("INSERT INTO devices(id,user_id,name,platform,public_key) VALUES($1,$2,'test','linux','public')",[p.deviceId,p.userId]);await admin.query("INSERT INTO sessions(id,user_id,device_id,token_hash,refresh_token_hash,access_expires_at,refresh_expires_at) VALUES($1,$2,$3,$4,$5,now()+interval '10 minutes',now()+interval '1 hour')",[p.sessionId,p.userId,p.deviceId,`access-${p.userId}`,`refresh-${p.userId}`]);}await admin.query("INSERT INTO workspaces(id,slug,name,created_by) VALUES($1,'sandbox-api','Sandbox API',$2)",[workspaceId,owner.userId]);await admin.query("INSERT INTO workspace_memberships(workspace_id,user_id,role) VALUES($1,$2,'owner'),($1,$3,'operator')",[workspaceId,owner.userId,other.userId]);const service=new SandboxService(new PgSandboxStore(runtime));const manifest=JSON.parse(await readFile(path.resolve(path.dirname(fileURLToPath(import.meta.url)),"../../../packages/contracts/testdata/sandbox/template-good.json"),"utf8")) as SandboxTemplateManifest;
    const created=await service.createTemplate(owner,workspaceId,"template-create-1",manifest);const published=await service.publishVersion(owner,created.template.id,"template-publish-1",{expectedDraftVersion:1});assert.equal(published.version.manifest.spec.version,manifest.spec.version);const mutation=await service.createSandbox(other,workspaceId,"sandbox-create-1",{template:{name:manifest.metadata.name,version:manifest.spec.version},architecture:"amd64",allocationMode:"direct",expiresInSeconds:900,sources:[{repository:"source",commit:"1".repeat(40)}],approvedNonSensitive:true});assert.equal(mutation.sandbox.requestedBy,other.userId);assert.equal(mutation.operation.type,"create");assert.deepEqual((await service.createSandbox(other,workspaceId,"sandbox-create-1",{template:{name:manifest.metadata.name,version:manifest.spec.version},architecture:"amd64",allocationMode:"direct",expiresInSeconds:900,sources:[{repository:"source",commit:"1".repeat(40)}],approvedNonSensitive:true})).sandbox.id,mutation.sandbox.id);
    await assert.rejects(service.getSandbox({...other,userId:"10000000-0000-4000-8000-000000000099"},mutation.sandbox.id),isCode("sandbox_not_found"));
    await admin.query("BEGIN");
    try {
      const receiptId="92000000-0000-4000-8000-000000000099";
      await admin.query("UPDATE sandboxes SET state='ready',backend_uid='api-test-backend',backend_resource_version='api-test-resource',admission_id='api-test-admission' WHERE id=$1",[mutation.sandbox.id]);
      await admin.query(`INSERT INTO sandbox_workload_admissions(sandbox_id,workspace_id,operation_id,backend_uid,backend_resource_version,
        api_version,namespace,workload_name,workload_uid,workload_resource_version,admitted_cluster_queue,
        owner_api_version,owner_kind,owner_name,owner_uid,owner_controller,workspace_label,sandbox_label,
        admitted,condition_type,condition_status,admission_digest)
        VALUES($1,$2,$3,'api-test-backend','api-test-resource','kueue.x-k8s.io/v1beta1','blazn-poc-sandboxes',$4,
        'api-test-admission','workload-resource-1','poc-cluster','agents.x-k8s.io/v1beta1','Sandbox',$1::uuid::text,
        'api-test-backend',true,$2::uuid::text,$1::uuid::text,true,'Admitted','True',
        sandbox_workload_admission_digest('kueue.x-k8s.io/v1beta1','blazn-poc-sandboxes',$4,'api-test-admission',
        'workload-resource-1','poc-cluster','agents.x-k8s.io/v1beta1','Sandbox',$1::uuid::text,'api-test-backend',true,$2::uuid::text,
        $1::uuid::text,true,'Admitted','True'))`,[mutation.sandbox.id,workspaceId,mutation.operation.id,`workload-${mutation.sandbox.id}`]);
      await admin.query(`INSERT INTO sandbox_operation_terminal_receipts(id,operation_id,workspace_id,sandbox_id,operation_type,status,
        cleanup_complete,artifact_export_complete,grants_revoked,backend_destroyed,backend_present,backend_uid,
        backend_resource_version,admission_digest)
        SELECT $1,$2,$3,$4,'create','succeeded',false,false,false,false,true,'api-test-backend','api-test-resource',admission_digest
        FROM sandbox_workload_admissions WHERE sandbox_id=$4`,[receiptId,mutation.operation.id,workspaceId,mutation.sandbox.id]);
      await admin.query("UPDATE sandbox_operations SET status='succeeded',terminal_receipt_id=$1,completed_at=clock_timestamp() WHERE id=$2",[receiptId,mutation.operation.id]);
      await admin.query("COMMIT");
    } catch(error) { await admin.query("ROLLBACK"); throw error; }
    const artifactId="80000000-0000-4000-8000-000000000099";
    await admin.query(`INSERT INTO sandbox_artifacts(id,workspace_id,sandbox_id,name,path,object_key,media_type,content_digest,size_bytes,exported_at)
      VALUES($1,$2,$3,'patch','/workspace/artifacts/change.patch',$4,'text/plain',$5,6,clock_timestamp())`,
      [artifactId,workspaceId,mutation.sandbox.id,`workspaces/${workspaceId}/sandboxes/${mutation.sandbox.id}/artifacts/patch`,"d".repeat(64)]);
    assert.equal((await service.getArtifact(other,artifactId)).id,artifactId);
    assert.deepEqual((await service.listArtifacts(other,mutation.sandbox.id)).items.map(item=>item.id),[artifactId]);
    await assert.rejects(runtime.query(`INSERT INTO sandbox_artifacts(id,workspace_id,sandbox_id,name,path,object_key,media_type,content_digest,size_bytes,exported_at)
      VALUES($1,$2,$3,'patch','/workspace/artifacts/change.patch',$4,'text/plain',$5,6,clock_timestamp())`,
      ["80000000-0000-4000-8000-000000000098",workspaceId,mutation.sandbox.id,`workspaces/${workspaceId}/sandboxes/${mutation.sandbox.id}/artifacts/patch`,"d".repeat(64)]),pgCode("42501"));
    const grant=await service.createAccessGrant(other,mutation.sandbox.id,{kind:"exec",expiresInSeconds:30});assert.match(grant.accessToken,/^[A-Za-z0-9_-]{43}$/);const stored=await admin.query("SELECT token_hash FROM sandbox_access_grants WHERE id=$1",[grant.grant.id]);assert.notEqual(stored.rows[0].token_hash,grant.accessToken);const stop=await service.createOperation(other,mutation.sandbox.id,"sandbox-stop-1",{type:"stop",expectedVersion:1});assert.equal(stop.sandbox.version,2);assert.equal(stop.operation.expectedSandboxVersion,1);
  }finally{await runtime.end();await admin.end();}});
function principal(last:string):SandboxPrincipal&{deviceId:string}{return{userId:`10000000-0000-4000-8000-00000000000${last}`,deviceId:`20000000-0000-4000-8000-00000000000${last}`,sessionId:`30000000-0000-4000-8000-00000000000${last}`,email:`user${last}@example.test`,displayName:`User ${last}`};}const workspaceId="40000000-0000-4000-8000-000000000001";function isCode(code:string){return(e:unknown)=>e instanceof SandboxHttpError&&e.code===code;}
function pgCode(code:string){return(error:unknown)=>typeof error==="object"&&error!==null&&"code" in error&&(error as {code:unknown}).code===code;}

import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { AgentHarnessService } from "./agent-harness-service.js";
import { PgAgentHarnessStore } from "./agent-harness-store.js";
import type { AgentHarnessPrincipal,JsonDocument } from "./agent-harness-types.js";
import { agentVersionDigest } from "./harness-contract.js";
import { AgentRunControllerService } from "./agent-run-controller-service.js";
import { PgAgentRunControllerStore } from "./agent-run-controller-store.js";
import { createDatabase } from "./db.js";
import { RunService } from "./run-service.js";
import { PgRunStore } from "./run-store.js";

const runtimeUrl=process.env.BLAZN_AGENT_RUN_TEST_DATABASE_URL,adminUrl=process.env.BLAZN_AGENT_RUN_TEST_ADMIN_DATABASE_URL;
const controllerUrl=process.env.BLAZN_AGENT_RUN_CONTROLLER_TEST_DATABASE_URL;
const fixtureDirectory=path.resolve(path.dirname(fileURLToPath(import.meta.url)),"../../../packages/contracts/testdata/harness");
const fixture=async(name:string)=>JSON.parse(await readFile(path.join(fixtureDirectory,name),"utf8")) as JsonDocument;

test("PostgreSQL Agent Run controller freezes compatibility and fences allocation, retry, finalization, and cancellation",{skip:!runtimeUrl||!adminUrl||!controllerUrl},async()=>{
  const runtime=createDatabase(runtimeUrl!),admin=createDatabase(adminUrl!),controllerDb=createDatabase(controllerUrl!);
  const api=new AgentRunControllerService(new PgAgentRunControllerStore(runtime));
  const controller=new AgentRunControllerService(new PgAgentRunControllerStore(controllerDb));
  const workspaceId="40000000-0000-4000-8000-000000000001",projectId=randomUUID();
  const principal:AgentHarnessPrincipal={userId:"10000000-0000-4000-8000-000000000099",email:"agent-run@example.test",displayName:"Agent Run"};
  try{
    await admin.query("INSERT INTO users(id,email,display_name,password_salt,password_hash) VALUES($1,$2,$3,'salt','hash')",[principal.userId,principal.email,principal.displayName]);
    await admin.query("INSERT INTO workspaces(id,slug,name,created_by) VALUES($1,'agent-run','Agent Run',$2)",[workspaceId,principal.userId]);
    await admin.query("INSERT INTO workspace_memberships(workspace_id,user_id,role) VALUES($1,$2,'owner')",[workspaceId,principal.userId]);
    await admin.query("INSERT INTO projects(id,workspace_id,slug,kind,name,created_by) VALUES($1,$2,'agent','agent','Agent',$3)",[projectId,workspaceId,principal.userId]);
    const agentBundle=await fixture("agent-good.json"),harnessBundle=await fixture("hermes-profile.json");
    const template=agentBundle.version as JsonDocument,templateRef=template.template as JsonDocument,templateId=randomUUID();
    const spec={variants:[{name:"linux-amd64",architecture:"amd64",imageIndex:`registry.invalid/agent@sha256:${"1".repeat(64)}`,imageDigest:`registry.invalid/agent@sha256:${"2".repeat(64)}`,placementProfile:"poc-linux-amd64-v1",command:["/bin/true"],resources:{}}]};
    await admin.query("BEGIN");
    await admin.query("INSERT INTO sandbox_templates(id,workspace_id,name,draft_spec,draft_digest,created_by) VALUES($1,$2,'agent-template',$3::jsonb,$4,$5)",[templateId,workspaceId,JSON.stringify(spec),"a".repeat(64),principal.userId]);
    await admin.query("INSERT INTO sandbox_template_versions(id,workspace_id,template_id,version,canonical_spec,spec,content_digest,created_by) VALUES($1,$2,$3,'1','{}'::bytea,$4::jsonb,$5,$6)",[templateRef.versionId,workspaceId,templateId,JSON.stringify(spec),String(templateRef.digest).slice(7),principal.userId]);
    await admin.query("INSERT INTO sandbox_template_version_variants(version_id,workspace_id,template_id,name,architecture,image_index_digest,image_child_digest,placement_profile,command,resources) VALUES($1,$2,$3,'linux-amd64','amd64',$4,$5,'poc-linux-amd64-v1',$6::jsonb,$7::jsonb)",[templateRef.versionId,workspaceId,templateId,`registry.invalid/agent@sha256:${"1".repeat(64)}`,`registry.invalid/agent@sha256:${"2".repeat(64)}`,JSON.stringify(["/bin/true"]),JSON.stringify({})]);
    await admin.query("INSERT INTO sandbox_template_version_status(version_id,workspace_id,template_id,status,changed_by) VALUES($1,$2,$3,'published',$4)",[templateRef.versionId,workspaceId,templateId,principal.userId]);
    await admin.query("UPDATE sandbox_templates SET current_published_version_id=$1 WHERE id=$2",[templateRef.versionId,templateId]);
    await admin.query("COMMIT");
    const harness=new AgentHarnessService(new PgAgentHarnessStore(runtime));
    const definition=harnessBundle.definition as JsonDocument,harnessVersion=harnessBundle.version as JsonDocument,profile=harnessBundle.profile as JsonDocument;
    await harness.createHarnessDefinition(principal,workspaceId,"agent-run-definition",{definition});
    await harness.publishHarnessVersion(principal,workspaceId,String(definition.id),"agent-run-version",{version:harnessVersion});
    await harness.createHarnessProfile(principal,workspaceId,"agent-run-profile",{profile});
    const agent=agentBundle.agent as JsonDocument,agentVersion=structuredClone(agentBundle.version as JsonDocument);
    agentVersion.allowedHarnessProfiles=[{id:profile.id,digest:profile.digest}];agentVersion.defaultHarnessProfileId=profile.id;agentVersion.digest=agentVersionDigest(agentVersion);
    await admin.query("INSERT INTO agents(id,workspace_id,owner_id,name,tags,status,version,created_by) VALUES($1,$2,$3,$4,$5::jsonb,'active',1,$3)",[agent.id,workspaceId,principal.userId,agent.name,JSON.stringify(agent.tags)]);
    await harness.publishAgentVersion(principal,workspaceId,String(agent.id),"agent-run-agent-version",{version:agentVersion});

    const runService=new RunService(new PgRunStore(runtime)),create=()=>runService.createRun(principal,workspaceId,projectId,randomUUID(),{kind:"agent.execute",proofClass:"sandbox",planDigest:`sha256:${"9".repeat(64)}`,inputArtifactIds:[],outputNames:["patch","summary"]});
    const first=(await create()).run;
    assert.equal(await api.enqueue(first.id,workspaceId,String(agentVersion.id),String(profile.id)),true);
    await admin.query("UPDATE harness_profiles SET status='disabled',document=jsonb_set(document,'{status}','\"disabled\"'::jsonb) WHERE id=$1",[profile.id]);
    const rejected=(await create()).run;assert.equal(await api.enqueue(rejected.id,workspaceId,String(agentVersion.id),String(profile.id)),false);
    await admin.query("UPDATE harness_profiles SET status='approved',document=jsonb_set(document,'{status}','\"approved\"'::jsonb) WHERE id=$1",[profile.id]);
    const claim=await controller.claim("agent-run-worker",30);assert.equal(claim?.runId,first.id);assert.equal(claim?.agentVersionDigest,agentVersion.digest);
    assert.ok(await controller.renew(first.id,"agent-run-worker",claim!.leaseToken,30));
    assert.equal(await controller.renew(first.id,"wrong-worker",claim!.leaseToken,30),undefined);
    const nodeId=await seedNode(admin,workspaceId,principal.userId),sandboxId=await seedSandbox(admin,workspaceId,principal.userId,templateId,String(templateRef.versionId),String(templateRef.digest).slice(7));
    assert.equal(await controller.bindSandbox(first.id,"wrong-worker",claim!.leaseToken,1,nodeId,sandboxId),false);
    assert.equal(await controller.bindSandbox(first.id,"agent-run-worker",claim!.leaseToken,1,nodeId,sandboxId),true);
    const artifactId=randomUUID(),patchId=randomUUID();await admin.query(`INSERT INTO artifacts(id,workspace_id,project_id,source_run_id,kind,media_type,name,status,digest,size_bytes,object_key,created_by) VALUES
      ($1,$2,$3,$4,'agent.summary','document','summary','ready',$5,1,$6,$7),
      ($8,$2,$3,$4,'agent.patch','document','patch','ready',$9,1,$10,$7)`,[artifactId,workspaceId,projectId,first.id,`sha256:${"8".repeat(64)}`,`workspaces/${workspaceId}/agent/${artifactId}`,principal.userId,patchId,`sha256:${"7".repeat(64)}`,`workspaces/${workspaceId}/agent/${patchId}`]);
    assert.equal(await controller.finalize(first.id,"wrong-worker",claim!.leaseToken,2,"succeeded",undefined,[artifactId,patchId],1,[]),false);
    assert.equal(await controller.finalize(first.id,"agent-run-worker",claim!.leaseToken,2,"succeeded",undefined,[artifactId,patchId],1,[]),true);
    const finished=await admin.query("SELECT status,node_id,sandbox_id FROM runs WHERE id=$1",[first.id]);assert.deepEqual(finished.rows[0],{status:"succeeded",node_id:nodeId,sandbox_id:sandboxId});

    const retryRun=(await create()).run;assert.equal(await api.enqueue(retryRun.id,workspaceId,String(agentVersion.id),String(profile.id)),true);
    const retryClaim=await controller.claim("agent-run-retry",30);assert.equal(retryClaim?.runId,retryRun.id);
    assert.equal(await controller.retry(retryRun.id,"agent-run-retry",retryClaim!.leaseToken,0,"adapter_unavailable"),"retry_scheduled");
    const recovered=await controller.claim("agent-run-recovered",30);assert.equal(recovered?.runId,retryRun.id);assert.equal(recovered?.attempt,2);
    await admin.query("UPDATE agent_run_jobs SET attempt_count=5,lease_expires_at=clock_timestamp()-interval '1 second' WHERE run_id=$1",[retryRun.id]);
    assert.equal(await controller.claim("agent-run-after-exhaustion",30),undefined);
    const exhausted=await admin.query("SELECT r.status,r.error_code,rr.receipt,j.completed_at,j.worker_id,m.error_code AS marker_error FROM runs r JOIN run_receipts rr ON rr.run_id=r.id JOIN agent_run_jobs j ON j.run_id=r.id JOIN agent_run_preallocation_failures m ON m.run_id=r.id WHERE r.id=$1",[retryRun.id]);
    assert.equal(exhausted.rows[0]?.status,"failed");assert.equal(exhausted.rows[0]?.error_code,"controller_attempts_exhausted");
    assert.equal(exhausted.rows[0]?.receipt.schemaVersion,"blazn.run/receipt/v1alpha1");assert.equal(exhausted.rows[0]?.marker_error,"controller_attempts_exhausted");assert.ok(exhausted.rows[0]?.completed_at);assert.equal(exhausted.rows[0]?.worker_id,null);
    assert.equal((await admin.query("SELECT count(*)::int AS count FROM run_events WHERE run_id=$1 AND type='agent.run.failed'",[retryRun.id])).rows[0]?.count,1);

    const cancelled=(await create()).run;assert.equal(await api.enqueue(cancelled.id,workspaceId,String(agentVersion.id),String(profile.id)),true);
    await runService.cancelRun(principal,workspaceId,projectId,cancelled.id,"cancel-agent-run",1);
    await controller.claim("agent-run-after-cancel",30);
    assert.ok((await admin.query("SELECT completed_at FROM agent_run_jobs WHERE run_id=$1",[cancelled.id])).rows[0]?.completed_at);
    await assert.rejects(()=>runtime.query("SELECT * FROM agent_run_jobs"),pgCode("42501"));
    await assert.rejects(()=>controllerDb.query("SELECT * FROM agent_run_bindings"),pgCode("42501"));
  }finally{await admin.query("DELETE FROM workspaces WHERE id=$1",[workspaceId]).catch(()=>{});await admin.query("DELETE FROM users WHERE id=$1",[principal.userId]).catch(()=>{});await Promise.all([runtime.end(),admin.end(),controllerDb.end()]);}
});

async function seedNode(admin:ReturnType<typeof createDatabase>,workspaceId:string,userId:string){const id=randomUUID();await admin.query("INSERT INTO nodes(id,workspace_id,name,kind,owner_user_id,machine_fingerprint,host_platform,host_architecture,lifecycle_state,trust_state,service_version,kubernetes_cluster_id,kubernetes_node_name,kubernetes_node_uid,kubernetes_resource_version) VALUES($1,$2,'agent-node','managed',$3,$4,'linux','amd64','active','verified','test','cluster','agent-node',$5,'1')",[id,workspaceId,userId,"3".repeat(64),randomUUID()]);await admin.query("INSERT INTO node_identities(id,node_id,public_key_fingerprint,public_key,signing_key_id,generation,status,issued_at,expires_at) VALUES($1,$2,$3,$4,'key',1,'active',now(),now()+interval '1 hour')",[randomUUID(),id,"4".repeat(64),"A".repeat(43)]);await admin.query("INSERT INTO node_capability_versions(id,node_id,version,digest,payload,observed_at) VALUES($1,$2,1,$3,'{}',now())",[randomUUID(),id,"5".repeat(64)]);await admin.query("UPDATE nodes SET current_identity_generation=1,current_identity_status='active',current_capability_version=1,agent_eligible=true WHERE id=$1",[id]);return id;}
async function seedSandbox(admin:ReturnType<typeof createDatabase>,workspaceId:string,userId:string,templateId:string,versionId:string,digest:string){const id=randomUUID();await admin.query(`INSERT INTO sandboxes(id,workspace_id,requested_by,template_id,template_version_id,template_name,template_version,template_digest,variant_name,image_index_digest,image_child_digest,architecture,allocation_mode,state,desired_state,queue_name,artifact_contract_digest,isolation,approved_non_sensitive,expires_at) VALUES($1,$2,$3,$4,$5,'agent-template','1',$6,'linux-amd64',$7,$8,'amd64','direct','ready','ready','agent-queue',$9,'approved-non-sensitive-poc',true,now()+interval '1 hour')`,[id,workspaceId,userId,templateId,versionId,digest,`registry.invalid/agent@sha256:${"1".repeat(64)}`,`registry.invalid/agent@sha256:${"2".repeat(64)}`,"6".repeat(64)]);return id;}
function pgCode(code:string){return(error:unknown)=>!!error&&typeof error==="object"&&"code" in error&&error.code===code;}

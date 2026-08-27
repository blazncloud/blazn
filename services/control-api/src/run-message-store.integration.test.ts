import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import test from "node:test";
import { createDatabase } from "./db.js";
import { RunService } from "./run-service.js";
import { PgRunStore } from "./run-store.js";
import { RunHttpError,type RunPrincipal } from "./run-types.js";

const runtimeUrl=process.env.BLAZN_RUN_TEST_DATABASE_URL,adminUrl=process.env.BLAZN_RUN_TEST_ADMIN_DATABASE_URL;

test("PostgreSQL Run executor claims, delivers, prioritizes steering, and recovers expired leases",{skip:!runtimeUrl||!adminUrl},async()=>{
  const runtime=createDatabase(runtimeUrl!),admin=createDatabase(adminUrl!),owner=principal(),workspaceId=randomUUID(),projectId=randomUUID();
  try{
    await admin.query("INSERT INTO users(id,email,display_name,password_salt,password_hash) VALUES($1,$2,$3,'salt','hash')",[owner.userId,owner.email,owner.displayName]);
    await admin.query("INSERT INTO workspaces(id,slug,name,created_by) VALUES($1,$2,'Message Claim Test',$3)",[workspaceId,`message-${owner.userId.slice(0,8)}`,owner.userId]);
    await admin.query("INSERT INTO workspace_memberships(workspace_id,user_id,role) VALUES($1,$2,'owner')",[workspaceId,owner.userId]);
    await admin.query("INSERT INTO projects(id,workspace_id,slug,kind,name,created_by) VALUES($1,$2,'agent','agent','Agent',$3)",[projectId,workspaceId,owner.userId]);
    const service=new RunService(new PgRunStore(runtime)),planDigest=`sha256:${"a".repeat(64)}`;
    const sandbox=(await service.createRun(owner,workspaceId,projectId,"message-sandbox-create",{kind:"agent.task",proofClass:"sandbox",planDigest,inputArtifactIds:[],outputNames:[]})).run;
    const sandboxPrompt=(await service.sendRunMessage(owner,workspaceId,projectId,sandbox.id,"message-sandbox-prompt",{kind:"prompt",content:"Clone the repository"})).message;
    const sandboxClaim=(await service.claimRunMessage(owner,workspaceId,projectId,sandbox.id,"message-sandbox-claim",{leaseSeconds:30})).claim!;
    assert.equal(sandboxClaim.message.id,sandboxPrompt.id);
    assert.equal((await service.deliverRunMessage(owner,workspaceId,projectId,sandbox.id,sandboxPrompt.id,"message-sandbox-deliver",sandboxClaim.claimId)).message.status,"delivered");
    const run=(await service.createRun(owner,workspaceId,projectId,"message-run-create",{kind:"agent.task",proofClass:"synthetic",planDigest,inputArtifactIds:[],outputNames:[]})).run;
    const prompt=(await service.sendRunMessage(owner,workspaceId,projectId,run.id,"message-prompt",{kind:"prompt",content:"Inspect the repository"})).message;
    await service.recordSyntheticProgress(owner,workspaceId,projectId,run.id,"message-run-start",{sequence:0,phase:"agent.start",percent:0});
    const followup=(await service.sendRunMessage(owner,workspaceId,projectId,run.id,"message-followup",{kind:"followup",content:"Then run tests",parentMessageId:prompt.id})).message;
    const steer=(await service.sendRunMessage(owner,workspaceId,projectId,run.id,"message-steer",{kind:"steer",content:"Do not change dependencies"})).message;
    await assert.rejects(()=>service.completeSyntheticRun(owner,workspaceId,projectId,run.id,"message-complete-pending",{expectedVersion:2,planDigest,artifactIds:[],summary:{steps:1,warnings:[]}}),isCode("message_conflict"));
    const controller=await admin.connect();try{await controller.query("BEGIN");await controller.query("INSERT INTO run_receipts(run_id,workspace_id,project_id,proof_class,outcome,plan_digest,receipt) VALUES($1,$2,$3,'synthetic','succeeded',$4,$5)",[run.id,workspaceId,projectId,planDigest,{schemaVersion:"blazn.run/receipt/v1alpha1",proofClass:"synthetic",outcome:"succeeded",planDigest,artifactIds:[],summary:{steps:1,warnings:[]}}]);await controller.query("UPDATE runs SET status='succeeded',version=version+1,completed_at=clock_timestamp() WHERE id=$1",[run.id]);await assert.rejects(()=>controller.query("COMMIT"),pgCode("23514"));}finally{await controller.query("ROLLBACK").catch(()=>{});controller.release();}
    const promptClaim=(await service.claimRunMessage(owner,workspaceId,projectId,run.id,"message-claim-prompt",{leaseSeconds:30})).claim!;
    assert.equal(promptClaim.message.id,prompt.id);
    assert.equal((await service.deliverRunMessage(owner,workspaceId,projectId,run.id,prompt.id,"message-deliver-prompt",promptClaim.claimId)).message.status,"delivered");
    const firstSteerClaim=(await service.claimRunMessage(owner,workspaceId,projectId,run.id,"message-claim-steer-1",{leaseSeconds:30})).claim!;
    assert.equal(firstSteerClaim.message.id,steer.id);
    await admin.query("UPDATE run_messages SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1",[steer.id]);
    const recoveredSteerClaim=(await service.claimRunMessage(owner,workspaceId,projectId,run.id,"message-claim-steer-1",{leaseSeconds:30})).claim!;
    assert.equal(recoveredSteerClaim.message.id,steer.id);
    assert.equal(recoveredSteerClaim.claimId,firstSteerClaim.claimId);
    assert.notEqual(recoveredSteerClaim.leaseExpiresAt,firstSteerClaim.leaseExpiresAt);
    await service.deliverRunMessage(owner,workspaceId,projectId,run.id,steer.id,"message-deliver-steer",recoveredSteerClaim.claimId);
    const followupClaim=(await service.claimRunMessage(owner,workspaceId,projectId,run.id,"message-claim-followup",{leaseSeconds:30})).claim!;
    assert.equal(followupClaim.message.id,followup.id);
    await service.deliverRunMessage(owner,workspaceId,projectId,run.id,followup.id,"message-deliver-followup",followupClaim.claimId);
    assert.equal((await service.claimRunMessage(owner,workspaceId,projectId,run.id,"message-claim-empty",{leaseSeconds:30})).claim,null);
    assert.equal((await service.completeSyntheticRun(owner,workspaceId,projectId,run.id,"message-complete-delivered",{expectedVersion:2,planDigest,artifactIds:[],summary:{steps:1,warnings:[]}})).run.status,"succeeded");
    await assert.rejects(()=>runtime.query("UPDATE run_messages SET content='tampered' WHERE id=$1",[prompt.id]),pgCode("42501"));
    const stored=await admin.query<{status:string}>("SELECT status FROM run_messages WHERE run_id=$1 ORDER BY ordinal",[run.id]);
    assert.deepEqual(stored.rows.map(row=>row.status),["delivered","delivered","delivered"]);
  }finally{
    await admin.query("DELETE FROM workspaces WHERE id=$1",[workspaceId]).catch(()=>{});
    await admin.query("DELETE FROM users WHERE id=$1",[owner.userId]).catch(()=>{});
    await runtime.end();await admin.end();
  }
});

function principal():RunPrincipal{const userId=randomUUID();return{userId,email:`${userId}@example.test`,displayName:userId.slice(0,8)};}
function isCode(code:string){return(error:unknown)=>error instanceof RunHttpError&&error.code===code;}
function pgCode(code:string){return(error:unknown)=>!!error&&typeof error==="object"&&"code" in error&&error.code===code;}

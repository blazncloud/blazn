import assert from "node:assert/strict";
import { generateKeyPairSync, randomUUID } from "node:crypto";
import test from "node:test";
import { createDatabase } from "./db.js";
import { NodeService } from "./node-service.js";
import { PgNodeStore } from "./node-store.js";
import { NodeHttpError } from "./node-types.js";

const adminUrl=process.env.NODE_TEST_ADMIN_DATABASE_URL;
const runtimeUrl=process.env.NODE_TEST_RUNTIME_DATABASE_URL;

test("PostgreSQL serializes enrollment replay and isolates workspaces",{skip:!adminUrl||!runtimeUrl},async()=>{
  const admin=createDatabase(adminUrl!),runtime=createDatabase(runtimeUrl!);const userId=randomUUID(),outsiderId=randomUUID(),workspaceId=randomUUID(),otherWorkspaceId=randomUUID();
  try{
    await admin.query("INSERT INTO users(id,email,display_name,password_salt,password_hash) VALUES($1,$2,'Node Operator','salt','hash'),($3,$4,'Outsider','salt','hash')",[userId,`node-${userId}@example.test`,outsiderId,`node-${outsiderId}@example.test`]);
    await admin.query("INSERT INTO workspaces(id,slug,name,created_by) VALUES($1,$2,'Node Test',$3),($4,$5,'Other',$6)",[workspaceId,`node-${userId.slice(0,8)}`,userId,otherWorkspaceId,`other-${outsiderId.slice(0,8)}`,outsiderId]);
    await admin.query("INSERT INTO workspace_memberships(workspace_id,user_id,role) VALUES($1,$2,'operator'),($3,$4,'owner'),($3,$2,'viewer')",[workspaceId,userId,otherWorkspaceId,outsiderId]);
    const signer=generateKeyPairSync("ed25519");
    const planFactory={create:async(c:{planId:string;nodeId:string;enrollment:{id:string;workspaceId:string;idempotencyKey:string;createdBy:string;requestedName:string}})=>({schemaVersion:"nodes/v1alpha1",planId:c.planId,nodeId:c.nodeId,enrollmentId:c.enrollment.id,workspaceId:c.enrollment.workspaceId,idempotencyKey:c.enrollment.idempotencyKey,approvedBy:c.enrollment.createdBy,approvedAt:"2026-08-22T12:00:00.000Z",hostname:c.enrollment.requestedName,mode:"fresh",installProfile:"ubuntu-26.04-amd64-worker/v1",issuedAt:"2026-08-22T12:00:00.000Z",expiresAt:"2026-08-22T12:15:00.000Z",signingKeyId:"test/v1",digest:`sha256:${c.planId.replaceAll("-","").padEnd(64,"0")}`,signature:"a".repeat(86)})};
    const service=new NodeService(new PgNodeStore(runtime),async()=>Buffer.alloc(32,9),planFactory,()=>new Date("2026-08-22T12:00:00Z"));const principal={userId,email:"operator@example.test",displayName:"Operator"};
    const [first,second]=await Promise.all([service.createEnrollment(principal,workspaceId,"parallel-key",{name:"ben-fresh",mode:"fresh",platform:"linux",architecture:"amd64"}),service.createEnrollment(principal,workspaceId,"parallel-key",{name:"ben-fresh",mode:"fresh",platform:"linux",architecture:"amd64"})]);
    assert.equal(first.id,second.id);assert.equal(first.token,second.token);assert.equal([first.replayed,second.replayed].filter(Boolean).length,1);
    const publicJwk=signer.publicKey.export({format:"jwk"});const exchange={token:first.token,machineFingerprint:"b".repeat(64),nodePublicKey:publicJwk.x!,platform:"linux" as const,architecture:"amd64" as const};
    const [planA,planB]=await Promise.all([service.exchangeEnrollment(first.id,exchange),service.exchangeEnrollment(first.id,exchange)]);assert.deepEqual(planA,planB);
    const own=await service.listNodes(principal,workspaceId);assert.equal(own.items.length,1);
    assert.equal((await service.listNodes(principal,otherWorkspaceId)).items.length,0);
    await assert.rejects(()=>service.createEnrollment(principal,otherWorkspaceId,"denied-key",{name:"ben-denied",mode:"fresh",platform:"linux",architecture:"amd64"}),(e:unknown)=>e instanceof NodeHttpError&&e.code==="permission_denied");
    const leaked=await admin.query("SELECT response_body::text body FROM workspace_idempotency_receipts WHERE principal_id=$1",[userId]);assert.equal(leaked.rows.some(r=>r.body.includes(first.token)),false);
  }finally{
    await admin.query("DELETE FROM workspaces WHERE id=ANY($1::uuid[])",[[workspaceId,otherWorkspaceId]]).catch(()=>{});await admin.query("DELETE FROM users WHERE id=ANY($1::uuid[])",[[userId,outsiderId]]).catch(()=>{});await runtime.end();await admin.end();
  }
});

import assert from "node:assert/strict";
import { generateKeyPairSync, sign } from "node:crypto";
import test from "node:test";
import { canonicalJson, enrollmentToken, renderedDigest } from "./node-crypto.js";
import { NodeService, type HeartbeatInput } from "./node-service.js";
import type { NodeIdempotencyReceipt, NodeStore, NodeTransaction } from "./node-store.js";
import { NodeHttpError } from "./node-types.js";

const workspaceId="11111111-1111-4111-8111-111111111111",userId="22222222-2222-4222-8222-222222222222",nodeId="33333333-3333-4333-8333-333333333333";
const principal={userId,email:"operator@example.test",displayName:"Operator"};
const planFactory={create:async()=>({digest:`sha256:${"a".repeat(64)}`,signature:"b".repeat(86),signingKeyId:"test/v1"})};

test("enrollment is authorized before replay and reconstructs no stored secret",async()=>{
  const key=Buffer.alloc(32,7);let authorized=true;const receipts=new Map<string,NodeIdempotencyReceipt>();let insertedTokenHash="";let enrollmentId="";
  const tx=baseTx({
    authority:async()=>authorized?{workspaceId,role:"operator",workspaceStatus:"active"}:undefined,
    getIdempotency:async(_p,_o,k)=>receipts.get(k),
    putIdempotency:async(_p,_o,k,r)=>{receipts.set(k,r);},
    insertEnrollment:async(v)=>{insertedTokenHash=v.tokenHash;enrollmentId=v.id;},
  });
  const service=new NodeService(store(tx),async()=>key,planFactory,()=>new Date("2026-08-22T12:00:00Z"));
  const first=await service.createEnrollment(principal,workspaceId,"same-key-1",{name:"ben-new",mode:"fresh",platform:"linux",architecture:"amd64"});
  const replay=await service.createEnrollment(principal,workspaceId,"same-key-1",{name:"ben-new",mode:"fresh",platform:"linux",architecture:"amd64"});
  assert.equal(replay.token,first.token);assert.equal(replay.replayed,true);assert.equal(first.token,enrollmentToken(key,workspaceId,enrollmentId,userId,"same-key-1"));assert.equal(insertedTokenHash.length,64);
  assert.equal(JSON.stringify([...receipts.values()]).includes(first.token),false);
  authorized=false;
  await assert.rejects(()=>service.createEnrollment(principal,workspaceId,"same-key-1",{name:"ben-new",mode:"fresh",platform:"linux",architecture:"amd64"}),(e:unknown)=>e instanceof NodeHttpError&&e.code==="membership_required");
});

test("heartbeat verifies identity, digest, sequence, and recursively rejects secrets",async()=>{
  const pair=generateKeyPairSync("ed25519");const jwk=pair.publicKey.export({format:"jwk"});const publicKey=jwk.x!;let prior:Awaited<ReturnType<NodeTransaction["heartbeatState"]>>;let writes=0;
  const binding={clusterId:"cluster-a",nodeName:"ben2",nodeUid:"uid-a",resourceVersion:"9"};
  const tx=baseTx({activeIdentity:async()=>({nodeId,workspaceId,generation:1,publicKey,publicKeyFingerprint:"c".repeat(64),signingKeyId:"node/v1",lifecycleState:"active",trustState:"verified",nodeVersion:1}),nodeById:async()=>({id:nodeId,workspaceId,name:"ben2",kind:"shared",platform:"linux",architecture:"amd64",lifecycleState:"active",trustState:"verified",agentEligible:true,version:1,capabilityVersion:null,identity:null,kubernetesBinding:binding,createdAt:"2026-08-22T00:00:00Z",updatedAt:"2026-08-22T00:00:00Z"}),heartbeatState:async()=>prior,recordHeartbeat:async(v)=>{prior={identityGeneration:v.identityGeneration,bootId:v.bootId,sequence:v.sequence,sentAt:v.sentAt,capabilityDigest:v.capabilityDigest};writes++;}});
  const service=new NodeService(store(tx),async()=>Buffer.alloc(32),planFactory,()=>new Date("2026-08-22T12:00:00Z"));
  const capability={version:1,host:{platform:"linux",architecture:"amd64",cpuMillis:1000,memoryBytes:1024,diskBytes:1024,accelerators:[],health:{status:"healthy",reasonCodes:[]}},worker:{platform:"linux",architecture:"amd64",allocatableCpuMillis:1000,allocatableMemoryBytes:1024,allocatableDiskBytes:1024,labels:{},limits:{maxConcurrentSandboxes:2,maxConcurrentAgents:2},health:{status:"healthy",reasonCodes:[]},kubernetesBinding:binding},sandboxBackends:[],runtimeClasses:[],localModels:[]};
  const body:HeartbeatInput={nodeId,identityGeneration:1,bootId:"boot-a",sequence:0,sentAt:"2026-08-22T12:00:00.000Z",capabilityDigest:renderedDigest("blazn-node-capability-v1",capability),capability};
  const proof=sign(null,Buffer.from(`blazn-node-heartbeat-v1\n${canonicalJson(body)}`),pair.privateKey).toString("base64url");
  await service.heartbeat(body,proof);assert.equal(writes,1);
  await assert.rejects(()=>service.heartbeat(body,proof),(e:unknown)=>e instanceof NodeHttpError&&e.code==="heartbeat_replay");
  const unsafe={...body,sequence:1,capability:{...capability,localModels:[{nested:{access_token:"leak"}}]}};unsafe.capabilityDigest=renderedDigest("blazn-node-capability-v1",unsafe.capability);const unsafeProof=sign(null,Buffer.from(`blazn-node-heartbeat-v1\n${canonicalJson(unsafe)}`),pair.privateKey).toString("base64url");
  await assert.rejects(()=>service.heartbeat(unsafe,unsafeProof),(e:unknown)=>e instanceof NodeHttpError&&e.code==="invalid_request");
});

test("operation checks version, role, state, and exact destructive binding",async()=>{
  const node={id:nodeId,workspaceId,name:"ben2",kind:"shared" as const,platform:"linux" as const,architecture:"amd64" as const,lifecycleState:"active" as const,trustState:"verified" as const,agentEligible:true,version:4,capabilityVersion:1,identity:null,kubernetesBinding:{clusterId:"cluster-a",nodeName:"ben2",nodeUid:"uid-a",resourceVersion:"9"},createdAt:"2026-08-22T00:00:00Z",updatedAt:"2026-08-22T00:00:00Z"};let inserts=0;
  const tx=baseTx({nodeById:async()=>node,authority:async()=>({workspaceId,role:"operator",workspaceStatus:"active"}),insertOperation:async(v)=>{inserts++;return{id:v.id,nodeId:v.nodeId,type:v.type,status:"pending",expectedNodeVersion:v.expectedVersion,result:null,error:null,receipt:null,createdAt:"2026-08-22T12:00:00Z"};}});
  const service=new NodeService(store(tx),async()=>Buffer.alloc(32),planFactory);
  await service.createOperation(principal,nodeId,"remove-key",{type:"remove",expectedVersion:4,parameters:{clusterId:"cluster-a",expectedNodeUid:"uid-a",expectedResourceVersion:"9",confirm:true,preserveHostData:true}});assert.equal(inserts,1);
  await assert.rejects(()=>service.createOperation(principal,nodeId,"wrong-vers",{type:"pause",expectedVersion:3,parameters:{}}),(e:unknown)=>e instanceof NodeHttpError&&e.code==="version_conflict");
});

function store(tx:NodeTransaction):NodeStore{return{transaction:async action=>action(tx)}}
function baseTx(overrides:Partial<NodeTransaction>):NodeTransaction{
  const unsupported=async()=>{throw new Error("unexpected fake transaction method")};
  return {lockIdempotency:async()=>{},getIdempotency:async()=>undefined,putIdempotency:async()=>{},authority:unsupported,insertEnrollment:unsupported,enrollmentById:unsupported,planByEnrollment:unsupported,createExchangedNode:unsupported,nodeById:unsupported,listNodes:unsupported,activeIdentity:unsupported,heartbeatState:async()=>undefined,recordHeartbeat:unsupported,insertOperation:unsupported,listEvents:unsupported,consumeJoin:unsupported,insertInstallReceipt:unsupported,completeOperation:unsupported,...overrides} as NodeTransaction;
}

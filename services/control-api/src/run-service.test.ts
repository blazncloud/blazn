import assert from "node:assert/strict";
import test from "node:test";
import { RunService } from "./run-service.js";
import type { RunStore,RunTransaction } from "./run-store.js";
import { RunHttpError,type Artifact,type ArtifactStatus,type Run,type RunAccess,type RunPrincipal,type RunReceipt,type RunStatus } from "./run-types.js";
import type { IdempotencyReceipt } from "./workspace-store.js";

const workspaceId="00000000-0000-4000-8000-000000000001",projectId="00000000-0000-4000-8000-000000000002",artifactId="00000000-0000-4000-8000-000000000005";
const owner:RunPrincipal={userId:"00000000-0000-4000-8000-000000000003",email:"owner@example.test",displayName:"Owner"};
const viewer:RunPrincipal={userId:"00000000-0000-4000-8000-000000000004",email:"viewer@example.test",displayName:"Viewer"};
const planDigest=`sha256:${"a".repeat(64)}`;

class MemoryRunStore implements RunStore,RunTransaction {
  runs=new Map<string,Run>();receipts=new Map<string,IdempotencyReceipt>();audits:string[]=[];
  artifacts=new Map<string,Artifact>([[artifactId,{id:artifactId,workspaceId,projectId,kind:"source",mediaType:"video",name:"source.mp4",status:"ready",version:1,digest:`sha256:${"b".repeat(64)}`,sizeBytes:12,createdBy:owner.userId,createdAt:"2026-08-22T00:00:00Z",updatedAt:"2026-08-22T00:00:00Z",downloadAvailable:true}]]);
  async transaction<T>(action:(tx:RunTransaction)=>Promise<T>){return action(this);}async lockIdempotency(){}
  async getIdempotency(p:string,o:string,k:string){return this.receipts.get(`${p}:${o}:${k}`);}async putIdempotency(p:string,o:string,k:string,r:IdempotencyReceipt){this.receipts.set(`${p}:${o}:${k}`,r);}
  async getAccess(w:string,p:string,u:string):Promise<RunAccess|undefined>{if(w!==workspaceId)return undefined;const access:RunAccess={workspaceStatus:"active",role:u===viewer.userId?"viewer":"owner"};if(p===projectId)access.projectStatus="active";return access;}
  async createRun(i:{id:string;workspaceId:string;projectId:string;kind:string;proofClass:string;planDigest:string;outputNames:string[];requestedBy:string;inputArtifactIds:string[]}){const run:Run={...i,proofClass:i.proofClass as Run["proofClass"],status:"queued",version:1,placement:null,receipt:null,createdAt:"2026-08-22T00:00:00Z"};this.runs.set(i.id,run);return run;}
  async getRun(w:string,p:string,id:string){const run=this.runs.get(id);return run?.workspaceId===w&&run.projectId===p?run:undefined;}
  async listRuns(w:string,p:string,status:RunStatus|"all"){const items=[...this.runs.values()].filter(r=>r.workspaceId===w&&r.projectId===p&&(status==="all"||r.status===status));return{items,nextCursor:null};}
  async cancelRun(current:Run){const receipt:RunReceipt={schemaVersion:"blazn.run/receipt/v1alpha1",proofClass:current.proofClass,outcome:"cancelled",planDigest:current.planDigest,artifactIds:[],summary:{steps:0,warnings:[]}};const run:Run={...current,status:"cancelled",version:current.version+1,completedAt:"2026-08-22T00:01:00Z",receipt};this.runs.set(run.id,run);return run;}
  async getArtifact(w:string,p:string,id:string){const artifact=this.artifacts.get(id);return artifact?.workspaceId===w&&artifact.projectId===p?artifact:undefined;}
  async listArtifacts(w:string,p:string,status:ArtifactStatus|"all"){return{items:[...this.artifacts.values()].filter(a=>a.workspaceId===w&&a.projectId===p&&(status==="all"||a.status===status)),nextCursor:null};}
  async insertAudit(_id:string,_w:string,_u:string,type:string){this.audits.push(type);}
}

test("Run creation is idempotent, tenant-bound, and validates structured inputs",async()=>{const store=new MemoryRunStore(),service=new RunService(store);const input={kind:"content.render",proofClass:"synthetic" as const,planDigest,inputArtifactIds:[artifactId],outputNames:["preview.mp4"]};const first=await service.createRun(owner,workspaceId,projectId,"run-create-1",input),replay=await service.createRun(owner,workspaceId,projectId,"run-create-1",input);assert.equal(first.run.id,replay.run.id);assert.deepEqual(first.run.inputArtifactIds,[artifactId]);assert.deepEqual(store.audits,["run.created"]);await assert.rejects(()=>service.createRun(owner,workspaceId,projectId,"run-create-1",{...input,kind:"content.other"}),isCode("idempotency_conflict"));await assert.rejects(()=>service.createRun(owner,workspaceId,projectId,"run-create-2",{...input,outputNames:["same","same"]}),isCode("invalid_request"));await assert.rejects(()=>service.createRun(owner,workspaceId,"00000000-0000-4000-8000-000000000099","run-create-3",input),isCode("project_not_found"));});

test("Run reads allow viewers while create and cancellation require operate",async()=>{const store=new MemoryRunStore(),service=new RunService(store),input={kind:"content.render",proofClass:"synthetic" as const,planDigest,inputArtifactIds:[],outputNames:[]};const created=await service.createRun(owner,workspaceId,projectId,"run-create-4",input);assert.equal((await service.listRuns(viewer,workspaceId,projectId)).items.length,1);assert.equal((await service.getRun(viewer,workspaceId,projectId,created.run.id)).run.id,created.run.id);assert.equal((await service.getArtifact(viewer,workspaceId,projectId,artifactId)).artifact.id,artifactId);await assert.rejects(()=>service.createRun(viewer,workspaceId,projectId,"run-create-5",input),isCode("permission_denied"));await assert.rejects(()=>service.cancelRun(viewer,workspaceId,projectId,created.run.id,"run-cancel-1",1),isCode("permission_denied"));});

test("Run cancellation records a bound terminal receipt and enforces optimistic versions",async()=>{const store=new MemoryRunStore(),service=new RunService(store),created=await service.createRun(owner,workspaceId,projectId,"run-create-6",{kind:"content.render",proofClass:"synthetic",planDigest,inputArtifactIds:[],outputNames:[]});const cancelled=await service.cancelRun(owner,workspaceId,projectId,created.run.id,"run-cancel-2",1);assert.equal(cancelled.run.status,"cancelled");assert.equal(cancelled.run.version,2);assert.equal(cancelled.run.receipt?.planDigest,planDigest);assert.equal((await service.cancelRun(owner,workspaceId,projectId,created.run.id,"run-cancel-2",1)).run.version,2);await assert.rejects(()=>service.cancelRun(owner,workspaceId,projectId,created.run.id,"run-cancel-3",1),isCode("version_conflict"));assert.deepEqual(store.audits,["run.created","run.cancelled"]);});

function isCode(code:string){return(error:unknown)=>error instanceof RunHttpError&&error.code===code;}

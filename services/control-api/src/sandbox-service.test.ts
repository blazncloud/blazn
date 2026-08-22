import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { SandboxService } from "./sandbox-service.js";
import type { SandboxIdempotencyReceipt,SandboxStore,SandboxTransaction } from "./sandbox-store.js";
import { SandboxHttpError,type SandboxAccessGrant,type SandboxPrincipal,type SandboxTemplate,type SandboxTemplateManifest,type SandboxView } from "./sandbox-types.js";

const here=path.dirname(fileURLToPath(import.meta.url));
const fixture=async()=>JSON.parse(await readFile(path.resolve(here,"../../../packages/contracts/testdata/sandbox/template-good.json"),"utf8")) as SandboxTemplateManifest;
const owner:SandboxPrincipal={userId:"10000000-0000-4000-8000-000000000001",sessionId:"30000000-0000-4000-8000-000000000001",email:"owner@example.test",displayName:"Owner"};
const member:SandboxPrincipal={userId:"10000000-0000-4000-8000-000000000002",sessionId:"30000000-0000-4000-8000-000000000002",email:"member@example.test",displayName:"Member"};
const workspaceId="40000000-0000-4000-8000-000000000001";

test("template mutations validate, persist no secret, replay idempotently, and redact drafts",async()=>{const memory=new MemoryStore();const service=new SandboxService(memory as unknown as SandboxStore);const manifest=await fixture();const created=await service.createTemplate(owner,workspaceId,"template-key-0001",manifest);const replay=await service.createTemplate(owner,workspaceId,"template-key-0001",manifest);assert.equal(replay.template.id,created.template.id);assert.equal(memory.templateCreates,1);assert.match(created.template.draftDigest!,/^sha256:[0-9a-f]{64}$/);assert.equal(JSON.stringify(memory.receipts).includes("accessToken"),false);
  memory.roles.set(member.userId,"member");memory.templates[0]!.publishedVersionId="50000000-0000-4000-8000-000000000001";const listed=await service.listTemplates(member,workspaceId);assert.equal(listed.items.length,1);assert.equal("draftManifest" in listed.items[0]!,false);assert.equal("draftDigest" in listed.items[0]!,false);
  const bad=structuredClone(manifest) as SandboxTemplateManifest&{spec:Record<string,unknown>};bad.spec.secret={value:"no"};await assert.rejects(service.createTemplate(owner,workspaceId,"template-key-0002",bad),isCode("invalid_request"));
});

test("sandbox access is requester-only with owner override audit and grants are fresh non-idempotent bearer material",async()=>{const memory=new MemoryStore();memory.sandboxValue=sandbox();const service=new SandboxService(memory as unknown as SandboxStore,()=>new Date("2026-08-22T00:00:00Z"));memory.roles.set(member.userId,"operator");await assert.rejects(service.getSandbox(member,memory.sandboxValue.id),isCode("sandbox_not_found"));const own={...memory.sandboxValue,requestedBy:member.userId};memory.sandboxValue=own;const first=await service.createAccessGrant(member,own.id,{kind:"exec",expiresInSeconds:30});const second=await service.createAccessGrant(member,own.id,{kind:"exec",expiresInSeconds:30});assert.notEqual(first.grant.id,second.grant.id);assert.notEqual(first.accessToken,second.accessToken);assert.equal(memory.grants.length,2);assert.equal(JSON.stringify(memory.grants).includes(first.accessToken),false);
  memory.sandboxValue={...own,requestedBy:member.userId};await service.getSandbox(owner,own.id);assert.equal(memory.audits.some(a=>a.override===true),true);
});

test("idempotency replay rechecks current role",async()=>{const memory=new MemoryStore();const service=new SandboxService(memory as unknown as SandboxStore);const manifest=await fixture();await service.createTemplate(owner,workspaceId,"template-key-0003",manifest);memory.roles.set(owner.userId,"viewer");await assert.rejects(service.createTemplate(owner,workspaceId,"template-key-0003",manifest),isCode("permission_denied"));});

class MemoryStore {
  readonly roles=new Map([[owner.userId,"owner"]]);readonly receipts=new Map<string,SandboxIdempotencyReceipt>();readonly templates:SandboxTemplate[]=[];readonly grants:SandboxAccessGrant[]=[];readonly audits:{type:string;override:boolean}[]=[];templateCreates=0;sandboxValue?:SandboxView;
  async transaction<T>(fn:(tx:SandboxTransaction)=>Promise<T>){return fn(this as unknown as SandboxTransaction);}async lockIdempotency(){}async getIdempotency(p:string,o:string,k:string){return this.receipts.get(`${p}:${o}:${k}`);}async putIdempotency(p:string,o:string,k:string,r:SandboxIdempotencyReceipt){this.receipts.set(`${p}:${o}:${k}`,r);}async membership(_w:string,u:string){const role=this.roles.get(u);return role?{role,status:"active"}:undefined;}
  async lockWorkspace(){return true;}
  async createTemplate(id:string,w:string,n:string,spec:SandboxTemplateManifest["spec"],digest:string){this.templateCreates++;const t:SandboxTemplate={id,workspaceId:w,name:n,draftVersion:1,draftManifest:{apiVersion:"blazn.dev/v1alpha1",kind:"SandboxTemplate",metadata:{name:n},spec},draftDigest:`sha256:${digest}`,publishedVersionId:null,createdAt:"2026-08-22T00:00:00.000Z",updatedAt:"2026-08-22T00:00:00.000Z"};this.templates.push(t);return t;}async listTemplates(){return{items:this.templates,nextCursor:null};}async template(id:string){return this.templates.find(t=>t.id===id);}async sandbox(){return this.sandboxValue;}
  async sessionState(){return{revokedAt:null,accessExpired:false};}
  async createGrant(i:{id:string;workspaceId:string;sandboxId:string;kind:"exec"|"upload"|"download";expiresInSeconds:number}){const g:SandboxAccessGrant={id:i.id,workspaceId:i.workspaceId,sandboxId:i.sandboxId,kind:i.kind,scope:`sandbox.${i.kind}`,state:"active",expiresAt:new Date(Date.parse("2026-08-22T00:00:00Z")+i.expiresInSeconds*1000).toISOString(),createdAt:"2026-08-22T00:00:00.000Z"};this.grants.push(g);return g;}async insertAudit(_id:string,_w:string,_s:string|undefined,_u:string,_session:string,type:string,override:boolean){this.audits.push({type,override});}
}
function sandbox():SandboxView{return{id:"70000000-0000-4000-8000-000000000001",workspaceId,requestedBy:owner.userId,templateId:"50000000-0000-4000-8000-000000000001",templateVersionId:"60000000-0000-4000-8000-000000000001",templateName:"coding-small",templateVersion:"bootstrap-1",templateDigest:`sha256:${"a".repeat(64)}`,variantName:"linux-amd64",imageIndexDigest:`registry.invalid/poc@sha256:${"b".repeat(64)}`,imageDigest:`registry.invalid/poc@sha256:${"c".repeat(64)}`,architecture:"amd64",allocationMode:"direct",sourceBindings:[],artifactContract:{digest:`sha256:${"d".repeat(64)}`,items:[]},state:"ready",desiredState:"ready",version:1,queueName:"blazn-poc",admissionId:null,isolation:"approved-non-sensitive-poc",expiresAt:"2026-08-22T00:15:00.000Z",conditions:[],createdAt:"2026-08-22T00:00:00.000Z",updatedAt:"2026-08-22T00:00:00.000Z"};}
function isCode(code:string){return(e:unknown)=>e instanceof SandboxHttpError&&e.code===code;}

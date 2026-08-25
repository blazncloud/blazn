import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { DevelopmentControllerService,DevelopmentControllerValidationError } from "./development-controller-service.js";
import type { DevelopmentControllerStore,DevelopmentControllerWorkItem } from "./development-controller-store.js";

const fixture=path.resolve(path.dirname(fileURLToPath(import.meta.url)),"../../../packages/contracts/testdata/development/build-good.json");
test("development controller validates authority, redaction, lease fencing, and terminal bindings",async()=>{
  const document=JSON.parse(await readFile(fixture,"utf8")) as Record<string,unknown>;
  const claim:DevelopmentControllerWorkItem={buildId:String(document.id),workspaceId:String(document.workspaceId),projectId:String(document.projectId),runId:String(document.runId),
    buildVersion:Number(document.version)-1,leaseToken:"90000000-0000-4000-8000-000000000001",leaseExpiresAt:"2026-08-22T00:10:00.000Z",attempt:1,
    requestedBy:String(document.requestedBy),source:document.source as {repository:string;commit:string},projectManifestDigest:String(document.projectManifestDigest),
    projectSnapshot:JSON.parse(await readFile(path.resolve(path.dirname(fixture),"project-good.json"),"utf8")),planDigest:String(document.planDigest),createdAt:String(document.createdAt)};
  const store=new MemoryStore(claim),service=new DevelopmentControllerService(store);
  assert.equal((await service.claim("builder-a",30))?.buildId,claim.buildId);
  assert.equal(await service.renew(claim.buildId,"builder-a",claim.leaseToken,30),claim.leaseExpiresAt);
  const execution={nodeId:"90000000-0000-4000-8000-000000000002",sandboxId:"90000000-0000-4000-8000-000000000003"};
  document.source={commit:claim.source.commit,repository:claim.source.repository};
  assert.equal(await service.finalize(claim.buildId,"builder-a",claim.leaseToken,claim.buildVersion,execution,document),true);
  assert.equal(store.finalized,1);
  const stale=new DevelopmentControllerService(new MemoryStore(undefined));
  assert.equal(await stale.finalize(claim.buildId,"builder-a",claim.leaseToken,claim.buildVersion,execution,document),false);
  for(const mutate of [
    (value:Record<string,unknown>)=>{value.workspaceId="40000000-0000-4000-8000-000000000099";},
    (value:Record<string,unknown>)=>{value.version=claim.buildVersion;},
    (value:Record<string,unknown>)=>{((value.evidence as Record<string,unknown>).secretScan as Record<string,unknown>).signedUrl="https://objects.invalid/a?X-Amz-Signature=abc";},
    (value:Record<string,unknown>)=>{((value.finalization as Record<string,unknown>).authority as Record<string,unknown>).principal="runtime-user";},
  ]){const candidate=structuredClone(document);mutate(candidate);await assert.rejects(()=>service.finalize(claim.buildId,"builder-a",claim.leaseToken,claim.buildVersion,execution,candidate),DevelopmentControllerValidationError);}
});

class MemoryStore implements DevelopmentControllerStore {
  finalized=0;constructor(private readonly item:DevelopmentControllerWorkItem|undefined){}
  async claim(){return this.item;}async renew(){return this.item?.leaseExpiresAt;}async resolve(){return this.item;}
  async finalize(){this.finalized++;return true;}
}

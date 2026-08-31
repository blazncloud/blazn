import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import test from "node:test";
import { AgentRunControllerService,AgentRunControllerValidationError } from "./agent-run-controller-service.js";
import type { AgentRunControllerStore } from "./agent-run-controller-store.js";

const id=()=>randomUUID(),store:AgentRunControllerStore={enqueue:async()=>true,claim:async()=>undefined,renew:async()=>new Date().toISOString(),bindSandbox:async()=>true,retry:async()=>"retry_scheduled",finalize:async()=>true};
test("Agent Run controller service validates the lease-fenced boundary",async()=>{
  const service=new AgentRunControllerService(store),run=id(),workspace=id(),version=id(),profile=id(),token=id(),node=id(),sandbox=id();
  assert.equal(await service.enqueue(run,workspace,version,profile),true);
  await service.claim("agent-controller-1",30);await service.renew(run,"agent-controller-1",token,30);
  assert.equal(await service.bindSandbox(run,"agent-controller-1",token,1,node,sandbox),true);
  assert.equal(await service.retry(run,"agent-controller-1",token,1,"adapter_unavailable"),"retry_scheduled");
  assert.equal(await service.finalize(run,"agent-controller-1",token,2,"succeeded",undefined,[],1,[]),true);
  assert.equal(await service.finalize(run,"agent-controller-1",token,2,"failed","harness_failed",[],1,[]),true);
  assert.equal(await service.finalize(run,"agent-controller-1",token,2,"failed","patch_failed",[id()],1,[]),true);
});
test("Agent Run controller service rejects malformed and inconsistent finalization",async()=>{
  const service=new AgentRunControllerService(store),run=id(),token=id();
  for(const action of [()=>service.claim("Bad Worker",30),()=>service.renew(run,"worker",token,9),()=>service.retry(run,"worker",token,-1,"bad"),
    ()=>service.finalize(run,"worker",token,1,"succeeded","unexpected",[],0,[]),()=>service.finalize(run,"worker",token,1,"failed",undefined,[],0,[])])
    assert.throws(action,AgentRunControllerValidationError);
});

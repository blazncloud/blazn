import assert from "node:assert/strict";
import type { AddressInfo } from "node:net";
import test from "node:test";
import { createNodeBrokerServer, internalBrokerErrorLine } from "./node-broker-http.js";
import type { NodeBrokerService } from "./node-broker-service.js";

const body={enrollmentId:"11111111-1111-4111-8111-111111111111",planId:"22222222-2222-4222-8222-222222222222",planDigest:`sha256:${"a".repeat(64)}`,nodeId:"33333333-3333-4333-8333-333333333333",machineFingerprint:"b".repeat(64),nodePublicKeyFingerprint:`sha256:${"c".repeat(64)}`};

test("broker HTTP accepts only bounded proof-authenticated issuance requests",async()=>{let calls=0;const service={issue:async()=>{calls++;return{issuanceId:"44444444-4444-4444-8444-444444444444",credential:"x".repeat(43),expiresAt:"2029-01-01T00:05:00.000Z",clusterId:"cluster-a",workerOnly:true,replayed:false};}} as unknown as NodeBrokerService,server=createNodeBrokerServer(service);await new Promise<void>(r=>server.listen(0,"127.0.0.1",r));const origin=`http://127.0.0.1:${(server.address() as AddressInfo).port}`;try{const accepted=await fetch(`${origin}/v1/node-service/join-credentials`,{method:"POST",headers:{"content-type":"application/json","idempotency-key":"join-key-1","x-blazn-node-proof":"x".repeat(86)},body:JSON.stringify(body)});assert.equal(accepted.status,200);assert.equal((await accepted.json() as {workerOnly:boolean}).workerOnly,true);assert.equal(calls,1);const bearer=await fetch(`${origin}/v1/node-service/join-credentials`,{method:"POST",headers:{"content-type":"application/json","authorization":"Bearer user-token","idempotency-key":"join-key-2","x-blazn-node-proof":"x".repeat(86)},body:JSON.stringify(body)});assert.equal(bearer.status,401);const large=await fetch(`${origin}/v1/node-service/join-credentials`,{method:"POST",headers:{"content-type":"application/json","idempotency-key":"join-key-3","x-blazn-node-proof":"x".repeat(86)},body:JSON.stringify({...body,padding:"x".repeat(17*1024)})});assert.equal(large.status,413);assert.equal(calls,1);}finally{await new Promise<void>(r=>server.close(()=>r()));}});

test("broker HTTP exposes only closed joined-worker observation",async()=>{let seen:unknown;const service={async observeJoin(value:unknown){seen=value;}} as unknown as NodeBrokerService,server=createNodeBrokerServer(service);await new Promise<void>(r=>server.listen(0,"127.0.0.1",r));const origin=`http://127.0.0.1:${(server.address() as AddressInfo).port}`,id="44444444-4444-4444-8444-444444444444",observation={clusterId:"cluster-a",nodeName:"worker-a",nodeUid:"uid-a",resourceVersion:"17"};try{const response=await fetch(`${origin}/v1/node-service/join-observations/${id}`,{method:"POST",headers:{"content-type":"application/json"},body:JSON.stringify(observation)});assert.equal(response.status,200);assert.deepEqual(await response.json(),{verified:true});assert.deepEqual(seen,{issuanceId:id,...observation});const extra=await fetch(`${origin}/v1/node-service/join-observations/${id}`,{method:"POST",headers:{"content-type":"application/json"},body:JSON.stringify({...observation,extra:true})});assert.equal(extra.status,400);}finally{await new Promise<void>(r=>server.close(()=>r()));}});

test("broker HTTP requires the exact caller key on every route when one is configured",async()=>{
  const key="k".repeat(43);let observed=0;
  const service={async health(){},async observeJoin(){observed++;}} as unknown as NodeBrokerService,server=createNodeBrokerServer(service,{callerKey:key});
  await new Promise<void>(r=>server.listen(0,"127.0.0.1",r));const origin=`http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  const observe=(headers:Record<string,string>)=>fetch(`${origin}/v1/node-service/join-observations/44444444-4444-4444-8444-444444444444`,{method:"POST",headers:{"content-type":"application/json",...headers},body:JSON.stringify({clusterId:"c",nodeName:"n",nodeUid:"u",resourceVersion:"1"})});
  try{
    for(const headers of [{},{"x-blazn-broker-caller":"wrong"},{"x-blazn-broker-caller":`${key}x`}]){const reply=await observe(headers);assert.equal(reply.status,401);assert.equal(((await reply.json()) as {code:string}).code,"unauthorized");}
    assert.equal((await fetch(`${origin}/healthz`)).status,401);
    assert.equal(observed,0);
    assert.equal((await observe({"x-blazn-broker-caller":key})).status,200);
    assert.equal((await fetch(`${origin}/healthz`,{headers:{"x-blazn-broker-caller":key}})).status,200);
    assert.equal(observed,1);
  }finally{await new Promise<void>(r=>server.close(()=>r()));}
});

test("internal broker errors log a bounded single line without multiline content",()=>{
  const error=Object.assign(new TypeError("bad\nvalue\u0000"),{code:"23514"});
  const line=internalBrokerErrorLine(error,"req-1");
  assert.equal(line,'node broker internal error requestId=req-1 name=TypeError code=23514 message="bad value?"');
  assert.match(internalBrokerErrorLine("x","r"),/name=UnknownError code=none message=""/);
  assert.equal(internalBrokerErrorLine(new Error("y".repeat(500)),"r").length<300,true);
});

test("broker HTTP removes a retired worker only through the closed retirement route",async()=>{
  let seen:unknown;const service={async retireNode(value:unknown){seen=value;return true;}} as unknown as NodeBrokerService,server=createNodeBrokerServer(service);
  await new Promise<void>(r=>server.listen(0,"127.0.0.1",r));const origin=`http://127.0.0.1:${(server.address() as AddressInfo).port}`,retirement={clusterId:"cluster-a",nodeName:"worker-a",nodeUid:"44444444-4444-4444-8444-444444444444"};
  try{
    const response=await fetch(`${origin}/v1/node-service/node-retirements`,{method:"POST",headers:{"content-type":"application/json"},body:JSON.stringify(retirement)});
    assert.equal(response.status,200);assert.deepEqual(await response.json(),{deleted:true});assert.deepEqual(seen,retirement);
    const extra=await fetch(`${origin}/v1/node-service/node-retirements`,{method:"POST",headers:{"content-type":"application/json"},body:JSON.stringify({...retirement,force:true})});assert.equal(extra.status,400);
    const bearer=await fetch(`${origin}/v1/node-service/node-retirements`,{method:"POST",headers:{"content-type":"application/json",authorization:"Bearer user-token"},body:JSON.stringify(retirement)});assert.equal(bearer.status,401);
    assert.equal((await fetch(`${origin}/v1/node-service/node-retirements`)).status,405);
  }finally{await new Promise<void>(r=>server.close(()=>r()));}
});

test("broker HTTP assigns a worker's workspace only through the closed assignment route",async()=>{
  let seen:unknown;const service={async assignNode(value:unknown){seen=value;return true;}} as unknown as NodeBrokerService,server=createNodeBrokerServer(service);
  await new Promise<void>(r=>server.listen(0,"127.0.0.1",r));const origin=`http://127.0.0.1:${(server.address() as AddressInfo).port}`,assignment={clusterId:"cluster-a",nodeName:"worker-a",nodeUid:"44444444-4444-4444-8444-444444444444",workspaceId:"55555555-5555-4555-8555-555555555555"};
  try{
    const response=await fetch(`${origin}/v1/node-service/node-assignments`,{method:"POST",headers:{"content-type":"application/json"},body:JSON.stringify(assignment)});
    assert.equal(response.status,200);assert.deepEqual(await response.json(),{assigned:true});assert.deepEqual(seen,assignment);
    const extra=await fetch(`${origin}/v1/node-service/node-assignments`,{method:"POST",headers:{"content-type":"application/json"},body:JSON.stringify({...assignment,labels:{}})});assert.equal(extra.status,400);
    const bearer=await fetch(`${origin}/v1/node-service/node-assignments`,{method:"POST",headers:{"content-type":"application/json",authorization:"Bearer user-token"},body:JSON.stringify(assignment)});assert.equal(bearer.status,401);
  }finally{await new Promise<void>(r=>server.close(()=>r()));}
});

test("broker placement-hold route accepts an exact hold or release and rejects user credentials",async()=>{
  const seen:unknown[]=[];const service={async holdNode(value:unknown){seen.push(value);return true;}} as unknown as NodeBrokerService,server=createNodeBrokerServer(service);
  await new Promise<void>(r=>server.listen(0,"127.0.0.1",r));const origin=`http://127.0.0.1:${(server.address() as AddressInfo).port}`,hold={clusterId:"cluster-a",nodeName:"worker-a",nodeUid:"44444444-4444-4444-8444-444444444444",holdReason:"quarantined"};
  const post=(body:unknown,headers:Record<string,string>={})=>fetch(`${origin}/v1/node-service/node-placement-holds`,{method:"POST",headers:{"content-type":"application/json",...headers},body:JSON.stringify(body)});
  try{
    const held=await post(hold);assert.equal(held.status,200);assert.deepEqual(await held.json(),{changed:true});
    const released=await post({...hold,holdReason:""});assert.equal(released.status,200);
    assert.deepEqual(seen,[hold,{...hold,holdReason:""}]);
    assert.equal((await post({...hold,holdReason:7})).status,400);
    assert.equal((await post({clusterId:hold.clusterId,nodeName:hold.nodeName,nodeUid:hold.nodeUid})).status,400);
    assert.equal((await post({...hold,workspaceId:"x"})).status,400);
    assert.equal((await post(hold,{authorization:"Bearer user-token"})).status,401);
    assert.equal(seen.length,2);
  }finally{await new Promise<void>(r=>server.close(()=>r()));}
});

test("broker drain route accepts an exact binding and rejects user credentials",async()=>{
  const seen:unknown[]=[];const service={async drainNode(value:unknown){seen.push(value);return true;}} as unknown as NodeBrokerService,server=createNodeBrokerServer(service);
  await new Promise<void>(r=>server.listen(0,"127.0.0.1",r));const origin=`http://127.0.0.1:${(server.address() as AddressInfo).port}`,drain={clusterId:"cluster-a",nodeName:"worker-a",nodeUid:"44444444-4444-4444-8444-444444444444"};
  const post=(body:unknown,headers:Record<string,string>={})=>fetch(`${origin}/v1/node-service/node-drains`,{method:"POST",headers:{"content-type":"application/json",...headers},body:JSON.stringify(body)});
  try{
    const drained=await post(drain);assert.equal(drained.status,200);assert.deepEqual(await drained.json(),{drained:true});assert.deepEqual(seen,[drain]);
    assert.equal((await post({...drain,holdReason:""})).status,400);
    assert.equal((await post(drain,{authorization:"Bearer user-token"})).status,401);
    assert.equal(seen.length,1);
  }finally{await new Promise<void>(r=>server.close(()=>r()));}
});

test("broker rebootstrap route accepts an exact binding and rejects user credentials",async()=>{
  const seen:unknown[]=[];const service={async rebootstrapNode(value:unknown){seen.push(value);return false;}} as unknown as NodeBrokerService,server=createNodeBrokerServer(service);
  await new Promise<void>(r=>server.listen(0,"127.0.0.1",r));const origin=`http://127.0.0.1:${(server.address() as AddressInfo).port}`,binding={clusterId:"cluster-a",nodeName:"worker-a",nodeUid:"44444444-4444-4444-8444-444444444444"};
  const post=(body:unknown,headers:Record<string,string>={})=>fetch(`${origin}/v1/node-service/node-rebootstraps`,{method:"POST",headers:{"content-type":"application/json",...headers},body:JSON.stringify(body)});
  try{
    const response=await post(binding);assert.equal(response.status,200);assert.deepEqual(await response.json(),{rebootstrapped:false});assert.deepEqual(seen,[binding]);
    assert.equal((await post({...binding,extra:1})).status,400);
    assert.equal((await post(binding,{authorization:"Bearer user-token"})).status,401);
  }finally{await new Promise<void>(r=>server.close(()=>r()));}
});

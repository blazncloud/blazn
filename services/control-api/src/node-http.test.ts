import assert from "node:assert/strict";
import { createServer } from "node:http";
import test from "node:test";
import { NodeHttpRouter } from "./node-http.js";
import type { NodeService } from "./node-service.js";
import { NodeHttpError } from "./node-types.js";

const workspaceId="11111111-1111-4111-8111-111111111111",nodeId="22222222-2222-4222-8222-222222222222",enrollmentId="33333333-3333-4333-8333-333333333333";

test("enrollment token stays in the bounded exchange body",async()=>{
  let observed="";let urlSeen="";let headersSeen="";
  const service={async exchangeEnrollment(_id:string,input:{token:string}){observed=input.token;return{plan:{schemaVersion:"nodes/v1alpha1"},identity:{generation:3,signingKeyId:"node-identity/v3",publicKeyFingerprint:`sha256:${"a".repeat(64)}`,issuedAt:"2026-08-22T00:00:00Z",expiresAt:"2026-09-22T00:00:00Z"}};}} as unknown as NodeService;
  const server=nodeServer(new NodeHttpRouter(service));await listen(server);
  try{const token="t".repeat(43);const response=await fetch(origin(server)+`/v1/node-enrollments/${enrollmentId}/exchange`,{method:"POST",headers:{"content-type":"application/json"},body:JSON.stringify({token,machineFingerprint:"a".repeat(64),nodePublicKey:"b".repeat(43),platform:"linux",architecture:"amd64"})});urlSeen=response.url;headersSeen=JSON.stringify(response.headers);assert.equal(response.status,200);assert.equal(observed,token);assert.doesNotMatch(urlSeen,new RegExp(token));assert.doesNotMatch(headersSeen,new RegExp(token));}finally{await close(server);}
});

test("node service proof is singular and never accepted from the JSON body",async()=>{
  let calls=0;const service={async heartbeat(){calls++;}} as unknown as NodeService;const server=nodeServer(new NodeHttpRouter(service));await listen(server);
  const body={nodeId,identityGeneration:1,bootId:"boot",sequence:0,sentAt:"2026-08-22T12:00:00Z",capabilityDigest:`sha256:${"a".repeat(64)}`,capability:{version:1}};
  try{const missing=await fetch(origin(server)+"/v1/node-service/heartbeats",{method:"POST",headers:{"content-type":"application/json"},body:JSON.stringify({...body,proof:"x".repeat(86)})});assert.equal(missing.status,400);assert.equal(calls,0);
    const accepted=await fetch(origin(server)+"/v1/node-service/heartbeats",{method:"POST",headers:{"content-type":"application/json","x-blazn-node-proof":"x".repeat(86)},body:JSON.stringify(body)});assert.equal(accepted.status,204);assert.equal(calls,1);
  }finally{await close(server);}
});

test("join consumption threads the required idempotency key",async()=>{let observed="";const service={async consumeJoin(_issuance:string,key:string){observed=key;return{id:nodeId};}} as unknown as NodeService;const server=nodeServer(new NodeHttpRouter(service));await listen(server);const body={nodeId,enrollmentId,planId:"55555555-5555-4555-8555-555555555555",joinedNodeUid:"uid-a",joinedNodeName:"node-a",resourceVersion:"1",clusterId:"cluster-a"};const url=origin(server)+`/v1/node-service/join-credentials/66666666-6666-4666-8666-666666666666/consume`;try{const missing=await fetch(url,{method:"POST",headers:{"content-type":"application/json","x-blazn-node-proof":"x".repeat(86)},body:JSON.stringify(body)});assert.equal(missing.status,400);const accepted=await fetch(url,{method:"POST",headers:{"content-type":"application/json","x-blazn-node-proof":"x".repeat(86),"idempotency-key":"consume-http-key"},body:JSON.stringify(body)});assert.equal(accepted.status,200);assert.equal(observed,"consume-http-key");}finally{await close(server);}});

test("recognized Node routes reject unsupported methods before identifiers",async()=>{const server=nodeServer(new NodeHttpRouter({} as NodeService));await listen(server);try{for(const path of [`/v1/workspaces/${workspaceId}/node-enrollments`,`/v1/node-enrollments/${enrollmentId}/exchange`,`/v1/workspaces/${workspaceId}/nodes`,`/v1/nodes/${nodeId}`,`/v1/nodes/${nodeId}/operations`,`/v1/nodes/${nodeId}/events`,"/v1/node-service/heartbeats",`/v1/node-service/join-credentials/${enrollmentId}/consume`]){const response=await fetch(origin(server)+path,{method:"OPTIONS"});assert.equal(response.status,405,path);assert.equal((await response.json() as {code:string}).code,"method_not_allowed");}}finally{await close(server);}});

function nodeServer(router:NodeHttpRouter){return createServer((request,response)=>{router.handle(request,response,new URL(request.url??"/","http://127.0.0.1"),async()=>({userId:"44444444-4444-4444-8444-444444444444",email:"user@example.test",displayName:"User"})).catch((error:unknown)=>{const failure=error instanceof NodeHttpError?error:new NodeHttpError("invalid_request","failed");if(!response.headersSent){response.writeHead(failure.status,{"content-type":"application/json"});response.end(JSON.stringify({code:failure.code}));}else response.end();});});}
function listen(server:ReturnType<typeof createServer>){return new Promise<void>(resolve=>server.listen(0,"127.0.0.1",resolve));}
function close(server:ReturnType<typeof createServer>){return new Promise<void>(resolve=>server.close(()=>resolve()));}
function origin(server:ReturnType<typeof createServer>){const a=server.address();if(!a||typeof a==="string")throw new Error("server unavailable");return`http://127.0.0.1:${a.port}`;}

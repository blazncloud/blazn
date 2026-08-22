import assert from "node:assert/strict";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import test from "node:test";
import { NodeHttpRouter } from "./node-http.js";
import type { NodeBrokerProxy } from "./node-broker-proxy.js";
import type { NodeService } from "./node-service.js";
import { NodeHttpError } from "./node-types.js";

const enrollmentId="33333333-3333-4333-8333-333333333333",planId="55555555-5555-4555-8555-555555555555",nodeId="22222222-2222-4222-8222-222222222222";
const body={enrollmentId,planId,planDigest:`sha256:${"a".repeat(64)}`,nodeId,machineFingerprint:"b".repeat(64),nodePublicKeyFingerprint:`sha256:${"c".repeat(64)}`};

test("public join issuance forwards only the exact proof contract",async()=>{
  let observed:unknown;const proxy={async issue(forwarded,key,proof){observed={body:forwarded,key,proof};return{status:200,body:Buffer.from('{"workerOnly":true}')}},async health(){}} as NodeBrokerProxy;
  const server=nodeServer(new NodeHttpRouter({} as NodeService,proxy));await listen(server);
  try{const accepted=await fetch(origin(server)+"/v1/node-service/join-credentials",{method:"POST",headers:{"content-type":"application/json","idempotency-key":"join-http-key","x-blazn-node-proof":"x".repeat(86)},body:JSON.stringify(body)});assert.equal(accepted.status,200);assert.deepEqual(observed,{body,key:"join-http-key",proof:"x".repeat(86)});
    const bearer=await fetch(origin(server)+"/v1/node-service/join-credentials",{method:"POST",headers:{"content-type":"application/json",authorization:"Bearer forbidden","idempotency-key":"join-http-key","x-blazn-node-proof":"x".repeat(86)},body:JSON.stringify(body)});assert.equal(bearer.status,401);
    const extra=await fetch(origin(server)+"/v1/node-service/join-credentials",{method:"POST",headers:{"content-type":"application/json","idempotency-key":"join-http-key","x-blazn-node-proof":"x".repeat(86)},body:JSON.stringify({...body,extra:true})});assert.equal(extra.status,400);
  }finally{await close(server);}
});

test("public join issuance fails closed without a loopback broker",async()=>{const server=nodeServer(new NodeHttpRouter({} as NodeService));await listen(server);try{const response=await fetch(origin(server)+"/v1/node-service/join-credentials",{method:"POST"});assert.equal(response.status,503);}finally{await close(server);}});

function nodeServer(router:NodeHttpRouter){return createServer((request,response)=>{router.handle(request,response,new URL(request.url??"/","http://127.0.0.1"),async()=>({userId:"44444444-4444-4444-8444-444444444444",email:"user@example.test",displayName:"User"})).catch((error:unknown)=>{const failure=error instanceof NodeHttpError?error:new NodeHttpError("invalid_request","failed");if(!response.headersSent){response.writeHead(failure.status,{"content-type":"application/json"});response.end(JSON.stringify({code:failure.code}));}else response.end();});});}
function listen(server:ReturnType<typeof createServer>){return new Promise<void>(resolve=>server.listen(0,"127.0.0.1",resolve));}
function close(server:ReturnType<typeof createServer>){return new Promise<void>(resolve=>server.close(()=>resolve()));}
function origin(server:ReturnType<typeof createServer>){return`http://127.0.0.1:${(server.address() as AddressInfo).port}`;}

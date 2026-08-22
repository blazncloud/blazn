import assert from "node:assert/strict";
import { createServer } from "node:http";
import test from "node:test";
import { SandboxHttpRouter } from "./sandbox-http.js";
import type { SandboxService } from "./sandbox-service.js";
import { SandboxHttpError,type SandboxPrincipal } from "./sandbox-types.js";

const principal:SandboxPrincipal={userId:"10000000-0000-4000-8000-000000000001",sessionId:"30000000-0000-4000-8000-000000000001",email:"owner@example.test",displayName:"Owner"};
const workspace="40000000-0000-4000-8000-000000000001",sandbox="70000000-0000-4000-8000-000000000001";

test("sandbox router maps exact operation identity and idempotency",async()=>{let received:unknown;const service={createOperation:async(...args:unknown[])=>{received=args;return{accepted:true};}};const router=new SandboxHttpRouter(service as unknown as SandboxService);const reply=await request(router,"POST",`/v1/sandboxes/${sandbox}/operations`,{"idempotency-key":"operation-key-1"},{type:"stop",expectedVersion:7});assert.equal(reply.status,202);assert.deepEqual(received,[principal,sandbox,"operation-key-1",{type:"stop",expectedVersion:7}]);});

test("access grant creation rejects idempotency and unsupported fields before issuing bearer material",async()=>{let calls=0;const service={createAccessGrant:async()=>{calls++;return{};}};const router=new SandboxHttpRouter(service as unknown as SandboxService);const withKey=await request(router,"POST",`/v1/sandboxes/${sandbox}/access-grants`,{"idempotency-key":"grant-key-0001"},{kind:"exec",expiresInSeconds:30});assert.equal(withKey.status,400);assert.equal(withKey.body.code,"invalid_request");const extra=await request(router,"POST",`/v1/sandboxes/${sandbox}/access-grants`,{}, {kind:"exec",expiresInSeconds:30,accessToken:"forbidden"});assert.equal(extra.status,400);assert.equal(calls,0);});

test("persistence router does not capture grant transport or artifact-content routes",()=>{const router=new SandboxHttpRouter({} as SandboxService);assert.equal(router.matches(`/v1/sandboxes/${sandbox}/events`),true);assert.equal(router.matches(`/v1/sandbox-access-grants/${sandbox}/exec`),false);assert.equal(router.matches(`/v1/sandbox-artifacts/${sandbox}/content`),false);assert.equal(router.matches(`/v1/workspaces/${workspace}/sandboxes`),true);});

async function request(router:SandboxHttpRouter,method:string,path:string,headers:Record<string,string>,body:unknown){const server=createServer(async(req,res)=>{try{await router.handle(req,res,new URL(req.url!,"http://localhost"),async()=>principal);}catch(e){const error=e instanceof SandboxHttpError?e:new SandboxHttpError("internal_error","request failed");const payload=JSON.stringify({code:error.code,message:error.message});res.writeHead(error.status,{"content-type":"application/json"});res.end(payload);}});await new Promise<void>(resolve=>server.listen(0,"127.0.0.1",resolve));try{const address=server.address();assert.ok(address&&typeof address==="object");const response=await fetch(`http://127.0.0.1:${address.port}${path}`,{method,headers:{"content-type":"application/json",...headers},body:JSON.stringify(body)});return{status:response.status,body:await response.json() as Record<string,unknown>};}finally{await new Promise<void>((resolve,reject)=>server.close(e=>e?reject(e):resolve()));}}

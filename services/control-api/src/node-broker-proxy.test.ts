import assert from "node:assert/strict";
import { createServer } from "node:http";
import test from "node:test";
import { LoopbackNodeBrokerProxy } from "./node-broker-proxy.js";

test("loopback proxy preserves exact request and bounded response without redirects",async()=>{
  let seen:unknown;const server=createServer((request,response)=>{if(request.url==="/healthz"){response.writeHead(200,{"content-type":"application/json"});return response.end('{"status":"ok"}');}const chunks:Buffer[]=[];request.on("data",(chunk:Buffer)=>chunks.push(chunk));request.on("end",()=>{seen={url:request.url,method:request.method,authorization:request.headers.authorization,idempotency:request.headers["idempotency-key"],proof:request.headers["x-blazn-node-proof"],body:JSON.parse(Buffer.concat(chunks).toString("utf8"))};response.writeHead(200,{"content-type":"application/json"});response.end('{"workerOnly":true}');});});
  await new Promise<void>((resolve,reject)=>{server.once("error",reject);server.listen(8081,"127.0.0.1",resolve);});
  try{const proxy=new LoopbackNodeBrokerProxy();await proxy.health(new AbortController().signal);const reply=await proxy.issue({nodeId:"node-a"},"join-key-1","x".repeat(86),new AbortController().signal);assert.equal(reply.status,200);assert.deepEqual(seen,{url:"/v1/node-service/join-credentials",method:"POST",authorization:undefined,idempotency:"join-key-1",proof:"x".repeat(86),body:{nodeId:"node-a"}});}finally{await new Promise<void>(resolve=>server.close(()=>resolve()));}
});

test("loopback proxy rejects redirect, oversized, and stalled broker responses",async()=>{
  for(const behavior of ["redirect","oversized","stalled"]){const server=createServer((_request,response)=>{if(behavior==="redirect"){response.writeHead(302,{location:"http://example.test", "content-type":"application/json"});response.end('{}');}else if(behavior==="oversized"){response.writeHead(200,{"content-type":"application/json"});response.end(JSON.stringify({padding:"x".repeat(17*1024)}));}});await new Promise<void>((resolve,reject)=>{server.once("error",reject);server.listen(8081,"127.0.0.1",resolve);});try{await assert.rejects(new LoopbackNodeBrokerProxy(behavior==="stalled"?20:1000).issue({},"join-key-1","x".repeat(86),new AbortController().signal));}finally{await new Promise<void>(resolve=>server.close(()=>resolve()));}}
});

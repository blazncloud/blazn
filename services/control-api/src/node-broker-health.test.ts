import assert from "node:assert/strict";
import type { AddressInfo } from "node:net";
import test from "node:test";
import { createNodeBrokerServer } from "./node-broker-http.js";
import { NodeBrokerService } from "./node-broker-service.js";
import type { NodeBrokerStore } from "./node-broker-store.js";
import type { WorkerCredentialIssuer } from "./node-broker-types.js";

test("broker health dynamically probes database, AES key, and issuer protocol",async()=>{
  let databaseHealthy=true,keyHealthy=true,issuerHealthy=true;
  const store={health:async()=>{if(!databaseHealthy)throw new Error("database detail")},transaction:async()=>{throw new Error("unused")}} as NodeBrokerStore;
  const issuer={health:async()=>{if(!issuerHealthy)throw new Error("issuer detail")}} as unknown as WorkerCredentialIssuer;
  const service=new NodeBrokerService(store,async()=>Buffer.alloc(keyHealthy?32:31),issuer),server=createNodeBrokerServer(service);
  await new Promise<void>(resolve=>server.listen(0,"127.0.0.1",resolve));const origin=`http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  try{assert.equal((await fetch(origin+"/healthz")).status,200);for(const fail of [()=>databaseHealthy=false,()=>{databaseHealthy=true;keyHealthy=false},()=>{keyHealthy=true;issuerHealthy=false}]){fail();const response=await fetch(origin+"/healthz");assert.equal(response.status,503);assert.doesNotMatch(await response.text(),/detail/);}issuerHealthy=true;assert.equal((await fetch(origin+"/healthz")).status,200);}finally{await new Promise<void>(resolve=>server.close(()=>resolve()));}
});

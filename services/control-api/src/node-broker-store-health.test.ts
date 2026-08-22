import assert from "node:assert/strict";
import test from "node:test";
import type { Database } from "./db.js";
import { probeNodeBrokerDatabase } from "./node-broker-store.js";

test("database health destroys an aborted client and recovers with a clean pooled client",async()=>{
  const releases:boolean[]=[];let rejectQuery:((error:Error)=>void)|undefined,calls=0;
  const blocked={query:()=>new Promise((_resolve,reject)=>{rejectQuery=reject}),release:(destroy?:boolean)=>{releases.push(Boolean(destroy));if(destroy)rejectQuery?.(new Error("socket destroyed"));}};
  const healthy={query:async()=>({rows:[{one:1}]}),release:(destroy?:boolean)=>releases.push(Boolean(destroy))};
  const database={connect:async()=>++calls===1?blocked:healthy} as unknown as Database;
  const controller=new AbortController(),pending=probeNodeBrokerDatabase(database,controller.signal);await new Promise(resolve=>setImmediate(resolve));controller.abort(new Error("deadline"));await assert.rejects(pending,/deadline/);assert.deepEqual(releases,[true]);
  await probeNodeBrokerDatabase(database,new AbortController().signal);assert.deepEqual(releases,[true,false]);
});

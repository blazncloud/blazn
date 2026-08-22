import assert from "node:assert/strict";
import test from "node:test";
import { endNodeBrokerDatabase, nodeBrokerDatabasePoolError, startNodeBroker } from "./node-broker-main.js";

test("broker startup uses the fixed issuer socket and fails closed when it is absent",async()=>{process.env.BLAZN_MICROK8S_ISSUER_SOCKET="/tmp/must-not-be-used.sock";await assert.rejects(()=>startNodeBroker(),/microk8s-worker-issuer\.sock/);delete process.env.BLAZN_MICROK8S_ISSUER_SOCKET;});

test("broker database pool errors expose only bounded name and code",()=>{const error=Object.assign(new Error("postgresql://broker:secret@example.test detail"),{code:"57P01"});const line=nodeBrokerDatabasePoolError(error);assert.equal(line,"Node broker database pool error name=Error code=57P01");assert.doesNotMatch(line,/secret|postgresql|detail|example/i);assert.equal(nodeBrokerDatabasePoolError(Object.assign(new Error("private"),{code:"invalid-code"})),"Node broker database pool error name=Error code=unknown");});

test("broker retains its pool error listener until shutdown settles",async()=>{const events:string[]=[],failure=Object.assign(new Error("private shutdown detail"),{code:"57P01"}),listener=(error:Error)=>events.push(`handled:${(error as Error&{code?:string}).code}`),database={end:async()=>{events.push("end");throw failure;},off:(event:"error",removed:(error:Error)=>void)=>{assert.equal(event,"error");assert.equal(removed,listener);events.push("off");}};await endNodeBrokerDatabase(database,listener);assert.deepEqual(events,["end","handled:57P01","off"]);});

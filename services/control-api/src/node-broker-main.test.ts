import assert from "node:assert/strict";
import test from "node:test";
import { nodeBrokerDatabasePoolError, startNodeBroker } from "./node-broker-main.js";

test("broker startup uses the fixed issuer socket and fails closed when it is absent",async()=>{process.env.BLAZN_MICROK8S_ISSUER_SOCKET="/tmp/must-not-be-used.sock";await assert.rejects(()=>startNodeBroker(),/microk8s-worker-issuer\.sock/);delete process.env.BLAZN_MICROK8S_ISSUER_SOCKET;});

test("broker database pool errors expose only bounded name and code",()=>{const error=Object.assign(new Error("postgresql://broker:secret@example.test detail"),{code:"57P01"});const line=nodeBrokerDatabasePoolError(error);assert.equal(line,"Node broker database pool error name=Error code=57P01");assert.doesNotMatch(line,/secret|postgresql|detail|example/i);assert.equal(nodeBrokerDatabasePoolError(Object.assign(new Error("private"),{code:"invalid-code"})),"Node broker database pool error name=Error code=unknown");});

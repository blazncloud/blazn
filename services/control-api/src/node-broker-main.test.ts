import assert from "node:assert/strict";
import test from "node:test";
import { startNodeBroker } from "./node-broker-main.js";

test("broker startup uses the fixed issuer socket and fails closed when it is absent",async()=>{delete process.env.BLAZN_MICROK8S_ISSUER_SOCKET;await assert.rejects(()=>startNodeBroker(),/microk8s-worker-issuer\.sock/);});

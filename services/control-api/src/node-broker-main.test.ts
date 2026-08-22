import assert from "node:assert/strict";
import test from "node:test";
import { startNodeBroker } from "./node-broker-main.js";

test("broker startup fails closed without a configured worker credential provider",async()=>{delete process.env.BLAZN_MICROK8S_ISSUER_SOCKET;await assert.rejects(()=>startNodeBroker(),/configuration is invalid/);});

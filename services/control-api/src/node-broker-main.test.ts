import assert from "node:assert/strict";
import test from "node:test";
import { startNodeBroker } from "./node-broker-main.js";

test("broker startup fails closed without an injected worker credential provider",async()=>{await assert.rejects(()=>startNodeBroker(),/requires an injected worker-only MicroK8s credential issuer/);});

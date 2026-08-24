import assert from "node:assert/strict";
import test from "node:test";
import { assertVerifierIdentity, verifierIdentity } from "../scripts/verifier-binding.mjs";

test("compiled verifier binding rejects stale source, schema, and output independently",()=>{const source=Buffer.from("source"),schema=Buffer.from("schema"),compiled=Buffer.from("compiled"),identity=verifierIdentity(source,schema,compiled);assert.doesNotThrow(()=>assertVerifierIdentity(identity,source,schema,compiled));for(const values of [[Buffer.from("stale"),schema,compiled],[source,Buffer.from("stale"),compiled],[source,schema,Buffer.from("stale")]])assert.throws(()=>assertVerifierIdentity(identity,...values),/stale or substituted/);});

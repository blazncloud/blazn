import assert from "node:assert/strict";
import test from "node:test";
import { joinCredentialAad, openJoinCredential, sealJoinCredential } from "./node-broker-crypto.js";

const context={workspaceId:"11111111-1111-4111-8111-111111111111",enrollmentId:"22222222-2222-4222-8222-222222222222",planId:"33333333-3333-4333-8333-333333333333",nodeId:"44444444-4444-4444-8444-444444444444",issuanceId:"55555555-5555-4555-8555-555555555555",idempotencyKey:"join-key-1",requestDigest:"a".repeat(64)};

test("join credential AES-256-GCM uses the frozen AAD, key, nonce, and tag layout",()=>{const key=Buffer.alloc(32,1),credential="x".repeat(43),nonce=Buffer.alloc(12,2);assert.equal(joinCredentialAad(context).toString(),`blazn-node-join-credential-v1\n${context.workspaceId}\n${context.enrollmentId}\n${context.planId}\n${context.nodeId}\n${context.issuanceId}\n${context.idempotencyKey}\n${context.requestDigest}`);const sealed=sealJoinCredential(key,credential,context,nonce);assert.equal(sealed.subarray(0,12).equals(nonce),true);assert.equal(sealed.length,12+43+16);assert.equal(openJoinCredential(key,sealed,context),credential);assert.throws(()=>openJoinCredential(key,sealed,{...context,idempotencyKey:"other-key"}));const tampered=Buffer.from(sealed);tampered[12]=tampered[12]!^1;assert.throws(()=>openJoinCredential(key,tampered,context));});

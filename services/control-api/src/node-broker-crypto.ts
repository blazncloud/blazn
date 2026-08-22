import { createCipheriv, createDecipheriv, createHash, randomBytes } from "node:crypto";
import { readFile } from "node:fs/promises";
import { canonicalJson } from "./node-crypto.js";

export interface JoinCredentialContext {
  workspaceId: string; enrollmentId: string; planId: string; nodeId: string;
  issuanceId: string; idempotencyKey: string; requestDigest: string;
}

export function brokerRequestDigest(value: unknown): string { return createHash("sha256").update(canonicalJson(value)).digest("hex"); }
export function credentialHash(value: string): string { return createHash("sha256").update(value).digest("hex"); }

export function joinCredentialAad(context: JoinCredentialContext): Buffer {
  for(const value of [context.workspaceId,context.enrollmentId,context.planId,context.nodeId,context.issuanceId])if(!/^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(value))throw new Error("join credential UUID binding is invalid");
  if(context.idempotencyKey.length<8||context.idempotencyKey.length>128||!/^[0-9a-f]{64}$/.test(context.requestDigest))throw new Error("join credential retry binding is invalid");
  return Buffer.from(`blazn-node-join-credential-v1\n${context.workspaceId}\n${context.enrollmentId}\n${context.planId}\n${context.nodeId}\n${context.issuanceId}\n${context.idempotencyKey}\n${context.requestDigest}`,"utf8");
}

export function sealJoinCredential(key:Buffer,credential:string,context:JoinCredentialContext,nonce=randomBytes(12)):Buffer{
  if(key.length!==32||nonce.length!==12||credential.length<43||credential.length>4096)throw new Error("join credential encryption input is invalid");
  const cipher=createCipheriv("aes-256-gcm",key,nonce);cipher.setAAD(joinCredentialAad(context));const ciphertext=Buffer.concat([cipher.update(credential,"utf8"),cipher.final()]);return Buffer.concat([nonce,ciphertext,cipher.getAuthTag()]);
}

export function openJoinCredential(key:Buffer,sealed:Buffer,context:JoinCredentialContext):string{
  if(key.length!==32||sealed.length<29)throw new Error("join credential decryption input is invalid");const nonce=sealed.subarray(0,12),tag=sealed.subarray(sealed.length-16),ciphertext=sealed.subarray(12,sealed.length-16);const decipher=createDecipheriv("aes-256-gcm",key,nonce);decipher.setAAD(joinCredentialAad(context));decipher.setAuthTag(tag);return Buffer.concat([decipher.update(ciphertext),decipher.final()]).toString("utf8");
}

export async function readJoinCredentialKey(path:string):Promise<Buffer>{const value=await readFile(path);if(value.length!==32)throw new Error("join credential key must be exactly 32 raw bytes");return value;}

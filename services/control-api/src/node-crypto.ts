import { createHash, createHmac, createPrivateKey, createPublicKey, sign, verify } from "node:crypto";
import { readFile } from "node:fs/promises";

export function canonicalJson(value: unknown): string {
  if (value === null || typeof value === "boolean" || typeof value === "string") return JSON.stringify(value);
  if (typeof value === "number") {
    if (!Number.isFinite(value)) throw new Error("non-finite number cannot be canonicalized");
    return JSON.stringify(value);
  }
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (typeof value === "object") {
    const record = value as Record<string, unknown>;
    return `{${Object.keys(record).sort().map((key) => `${JSON.stringify(key)}:${canonicalJson(record[key])}`).join(",")}}`;
  }
  throw new Error("unsupported canonical JSON value");
}

export function sha256Hex(value: string | Buffer): string { return createHash("sha256").update(value).digest("hex"); }
export function renderedDigest(prefix: string, value: unknown): string {
  return `sha256:${sha256Hex(`${prefix}\n${canonicalJson(value)}`)}`;
}
export function requestDigest(value: unknown): string { return sha256Hex(canonicalJson(value)); }

export function enrollmentToken(key: Buffer, workspaceId: string, enrollmentId: string, principalId: string, idempotencyKey: string): string {
  if (key.length !== 32) throw new Error("node enrollment HMAC key must be exactly 32 bytes");
  return createHmac("sha256", key).update(`blazn-node-enrollment-v1\n${workspaceId}\n${enrollmentId}\n${principalId}\n${idempotencyKey}`).digest("base64url");
}

export function publicKeyFingerprint(publicKey: string): string {
  if (!/^[A-Za-z0-9_-]{43}$/.test(publicKey)) throw new Error("invalid raw Ed25519 public key");
  const raw = Buffer.from(publicKey, "base64url");
  if (raw.length !== 32) throw new Error("invalid raw Ed25519 public key");
  return sha256Hex(raw);
}

export function verifyNodeProof(publicKey: string, prefix: string, body: unknown, proof: string): boolean {
  try {
    if (!/^[A-Za-z0-9_-]{86}$/.test(proof)) return false;
    const key = createPublicKey({ key: { kty: "OKP", crv: "Ed25519", x: publicKey }, format: "jwk" });
    return verify(null, Buffer.from(`${prefix}\n${canonicalJson(body)}`, "utf8"), key, Buffer.from(proof, "base64url"));
  } catch { return false; }
}

export function verifyNodePlanSignature(publicKey: string, digest: string, signature: string): boolean {
  try {
    if (!/^[A-Za-z0-9_-]{43}$/.test(publicKey) || !/^sha256:[0-9a-f]{64}$/.test(digest) || !/^[A-Za-z0-9_-]{86}$/.test(signature)) return false;
    const key = createPublicKey({ key: { kty: "OKP", crv: "Ed25519", x: publicKey }, format: "jwk" });
    return verify(null, Buffer.from(`blazn-node-install-plan-v1\n${digest}`, "utf8"), key, Buffer.from(signature, "base64url"));
  } catch { return false; }
}

export function nodeInstallReceiptDigest(receipt: Record<string, unknown>): string {
  const unsigned = { ...receipt };
  delete unsigned.digest;
  delete unsigned.signature;
  return `sha256:${sha256Hex(canonicalJson(unsigned))}`;
}

export function verifyNodeInstallReceiptSignature(publicKey: string, digest: string, signature: string): boolean {
  try {
    if (!/^[A-Za-z0-9_-]{43}$/.test(publicKey) || !/^sha256:[0-9a-f]{64}$/.test(digest) || !/^[A-Za-z0-9_-]{86}$/.test(signature)) return false;
    const key = createPublicKey({ key: { kty: "OKP", crv: "Ed25519", x: publicKey }, format: "jwk" });
    return verify(null, Buffer.from(`blazn-node-install-receipt-v1\n${digest}`, "utf8"), key, Buffer.from(signature, "base64url"));
  } catch { return false; }
}

export interface NodePlanSigner {
  readonly keyId: string;
  publicKey(): Promise<{ keyId: string; publicKey: string; fingerprint: string }>;
  sign(unsignedPlan: Record<string, unknown>): Promise<Record<string, unknown>>;
  signActivationGrant(unsignedGrant: Record<string, unknown>): Promise<Record<string, unknown>>;
}

export class FileNodePlanSigner implements NodePlanSigner {
  constructor(readonly keyId: string, private readonly privateKeyFile: string) {
    if (!keyId || keyId.length > 128) throw new Error("node plan signing key ID is invalid");
  }
  async publicKey(): Promise<{ keyId: string; publicKey: string; fingerprint: string }> {
    const privateKey = await readEd25519PrivateKey(this.privateKeyFile);
    const jwk = createPublicKey(privateKey).export({ format: "jwk" });
    if (jwk.kty !== "OKP" || jwk.crv !== "Ed25519" || typeof jwk.x !== "string") throw new Error("node plan signing public key is invalid");
    return { keyId: this.keyId, publicKey: jwk.x, fingerprint: `sha256:${publicKeyFingerprint(jwk.x)}` };
  }
  async sign(unsignedPlan: Record<string, unknown>): Promise<Record<string, unknown>> {
    const normalized: Record<string, unknown> = { ...unsignedPlan, signingKeyId: this.keyId };
    delete normalized.digest; delete normalized.signature;
    const digest = `sha256:${sha256Hex(canonicalJson(normalized))}`;
    const key = await readEd25519PrivateKey(this.privateKeyFile);
    const signature = sign(null, Buffer.from(`blazn-node-install-plan-v1\n${digest}`, "utf8"), key).toString("base64url");
    return { ...normalized, digest, signature };
  }
  async signActivationGrant(unsignedGrant: Record<string, unknown>): Promise<Record<string, unknown>> {
    const normalized: Record<string, unknown> = { ...unsignedGrant, signingKeyId: this.keyId };
    delete normalized.digest; delete normalized.signature;
    const digest = `sha256:${sha256Hex(canonicalJson(normalized))}`;
    const key = await readEd25519PrivateKey(this.privateKeyFile);
    const signature = sign(null, Buffer.from(`blazn-node-capacity-activation-grant-v1\n${digest}`, "utf8"), key).toString("base64url");
    return { ...normalized, digest, signature };
  }
}

async function readEd25519PrivateKey(path: string) {
  const value=await readFile(path);let key;
  const text=value.toString("utf8");
  if(/^[A-Za-z0-9_-]{43}\n$/.test(text)){
    const seed=Buffer.from(text.slice(0,-1),"base64url");
    key=createPrivateKey({key:Buffer.concat([Buffer.from("302e020100300506032b657004220420","hex"),seed]),format:"der",type:"pkcs8"});
  }else if(text.startsWith("-----BEGIN PRIVATE KEY-----"))key=createPrivateKey(value);
  else key=createPrivateKey({key:value,format:"der",type:"pkcs8"});
  if(key.asymmetricKeyType!=="ed25519")throw new Error("node plan signing key must be Ed25519 PKCS8 or an exact raw seed");
  return key;
}

export async function readNodeEnrollmentKey(path: string): Promise<Buffer> {
  const value = await readFile(path);
  if (value.length !== 32) throw new Error("node enrollment HMAC key must be exactly 32 bytes");
  return value;
}

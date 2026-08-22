import { createHash, createHmac } from "node:crypto";
import { readFile } from "node:fs/promises";

export const invitationKeyId = "workspace-invitation-hmac/v1";

export function sha256(value: string): string {
  return createHash("sha256").update(value).digest("hex");
}

export function canonicalJson(value: unknown): string {
  if (value === null || typeof value !== "object") return JSON.stringify(value);
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  const entries = Object.entries(value as Record<string, unknown>).sort(([left], [right]) => left.localeCompare(right));
  return `{${entries.map(([key, child]) => `${JSON.stringify(key)}:${canonicalJson(child)}`).join(",")}}`;
}

export function requestDigest(value: unknown): string {
  return sha256(canonicalJson(value));
}

export function invitationToken(key: Buffer, workspaceId: string, invitationId: string, idempotencyKey: string): string {
  const canonical = `blazn-workspace-invite-v1\n${workspaceId.toLowerCase()}\n${invitationId.toLowerCase()}\n${idempotencyKey}`;
  return createHmac("sha256", key).update(canonical).digest("base64url");
}

export async function readInvitationKey(): Promise<Buffer> {
  const path = process.env.WORKSPACE_INVITATION_HMAC_KEY_FILE;
  if (!path) throw new Error("WORKSPACE_INVITATION_HMAC_KEY_FILE is required for invitation operations");
  const encoded = (await readFile(path, "utf8")).trim();
  if (!/^[a-f0-9]{64}$/.test(encoded)) throw new Error("workspace invitation HMAC key must be 64 lowercase hexadecimal characters");
  return Buffer.from(encoded, "hex");
}

import type { IncomingMessage, ServerResponse } from "node:http";

export async function jsonBody(request: IncomingMessage, limit = 64 * 1024): Promise<Record<string, unknown>> {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of request) {
    const bytes = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
    size += bytes.length;
    if (size > limit) throw new HttpError(413, "request_too_large", "request body is too large");
    chunks.push(bytes);
  }
  if (chunks.length === 0) return {};
  try {
    const value: unknown = JSON.parse(Buffer.concat(chunks).toString("utf8"));
    if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("object required");
    return value as Record<string, unknown>;
  } catch {
    throw new HttpError(400, "invalid_json", "request body must be a JSON object");
  }
}

export function sendJson(response: ServerResponse, status: number, body: unknown): void {
  const payload = JSON.stringify(body);
  response.writeHead(status, { "content-type": "application/json", "content-length": Buffer.byteLength(payload), "cache-control": "no-store" });
  response.end(payload);
}

export class HttpError extends Error {
  constructor(readonly status: number, readonly code: string, message: string) {
    super(message);
  }
}

export function requiredString(body: Record<string, unknown>, key: string, max = 256): string {
  const value = body[key];
  if (typeof value !== "string" || value.trim() === "" || value.length > max) {
    throw new HttpError(400, "invalid_request", `${key} must be a non-empty string of at most ${max} characters`);
  }
  return value.trim();
}

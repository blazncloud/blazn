import { randomUUID } from "node:crypto";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { NodeBrokerService } from "./node-broker-service.js";
import type { JoinCredentialRequest } from "./node-broker-types.js";
import { nodeErrorBody, NodeHttpError } from "./node-types.js";

export function createNodeBrokerServer(service: NodeBrokerService): Server {
  return createServer(async (request, response) => {
    const requestId = randomUUID();
    try {
      if (request.url === "/healthz" && request.method === "GET") { try { await service.health(AbortSignal.timeout(2_000)); return send(response, 200, { status: "ok" }); } catch { throw new NodeHttpError("node_broker_unavailable", "Node broker is unavailable"); } }
      if (request.url !== "/v1/node-service/join-credentials") throw new NodeHttpError("not_found", "broker route was not found");
      if (request.method !== "POST") throw new NodeHttpError("method_not_allowed", "method is not allowed for this route");
      if (request.headers.authorization !== undefined) throw new NodeHttpError("unauthorized", "user and management credentials are not accepted by the Node broker");
      const idempotency = single(request, "idempotency-key");
      if (idempotency.length < 8 || idempotency.length > 128) throw new NodeHttpError("invalid_request", "Idempotency-Key is invalid");
      const proof = single(request, "x-blazn-node-proof");
      if (!/^[A-Za-z0-9_-]{86}$/.test(proof)) throw new NodeHttpError("identity_rejected", "X-Blazn-Node-Proof is invalid");
      const body = await jsonBody(request);
      exact(body, ["enrollmentId", "planId", "planDigest", "nodeId", "machineFingerprint", "nodePublicKeyFingerprint"]);
      const input: JoinCredentialRequest = { enrollmentId: text(body.enrollmentId, "enrollmentId", 64), planId: text(body.planId, "planId", 64), planDigest: text(body.planDigest, "planDigest", 71), nodeId: text(body.nodeId, "nodeId", 64), machineFingerprint: text(body.machineFingerprint, "machineFingerprint", 64), nodePublicKeyFingerprint: text(body.nodePublicKeyFingerprint, "nodePublicKeyFingerprint", 71) };
      send(response, 200, await service.issue(idempotency, input, proof));
    } catch (error) {
      const failure = error instanceof NodeHttpError ? error : new NodeHttpError("internal_error", "internal broker error");
      if (failure.code === "rate_limited") response.setHeader("retry-after", "60");
      send(response, failure.status, nodeErrorBody(failure, requestId));
    }
  });
}

async function jsonBody(request: IncomingMessage): Promise<Record<string, unknown>> { const contentType = singleOptional(request, "content-type"); if (contentType !== "application/json") throw new NodeHttpError("invalid_request", "content-type must be application/json"); const chunks: Buffer[] = []; let size = 0; for await (const chunk of request) { const bytes = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk); size += bytes.length; if (size > 16 * 1024) throw new NodeHttpError("request_too_large", "request body is too large"); chunks.push(bytes); } try { const parsed: unknown = JSON.parse(Buffer.concat(chunks).toString("utf8")); if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error(); return parsed as Record<string, unknown>; } catch { throw new NodeHttpError("invalid_json", "request body must be a JSON object"); } }
function exact(value: Record<string, unknown>, keys: string[]): void { if (Object.keys(value).length !== keys.length || keys.some((key) => !(key in value)) || Object.keys(value).some((key) => !keys.includes(key))) throw new NodeHttpError("invalid_request", `request body must contain exactly: ${keys.join(", ")}`); }
function text(value: unknown, name: string, max: number): string { if (typeof value !== "string" || !value || value.length > max) throw new NodeHttpError("invalid_request", `${name} is invalid`); return value; }
function single(request: IncomingMessage, name: string): string { const values = request.headersDistinct[name] ?? []; if (values.length !== 1) throw new NodeHttpError("invalid_request", `${name} must appear exactly once`); return values[0]!; }
function singleOptional(request: IncomingMessage, name: string): string | undefined { const values = request.headersDistinct[name] ?? []; if (values.length > 1) throw new NodeHttpError("invalid_request", `${name} must not be repeated`); return values[0]; }
function send(response: ServerResponse, status: number, body: unknown): void { const payload = JSON.stringify(body); response.writeHead(status, { "content-type": "application/json", "content-length": Buffer.byteLength(payload), "cache-control": "no-store" }); response.end(payload); }

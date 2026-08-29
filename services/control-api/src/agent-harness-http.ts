import type { IncomingMessage, ServerResponse } from "node:http";
import { jsonBody, sendJson } from "./http.js";
import type { AgentHarnessService } from "./agent-harness-service.js";
import { AgentHarnessHttpError, type AgentHarnessPrincipal, type JsonDocument } from "./agent-harness-types.js";

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

export class AgentHarnessHttpRouter {
  constructor(private readonly service: AgentHarnessService) {}
  matches(pathname: string): boolean {
    return /^\/v1\/workspaces\/[^/]+\/(?:agents(?:\/[^/]+(?:\/versions(?:\/[^/]+)?)?)?|harness\/(?:definitions(?:\/[^/]+(?:\/versions(?:\/[^/]+)?)?)?|profiles(?:\/[^/]+(?:\/revisions)?)?))$/.test(pathname);
  }
  async handle(request: IncomingMessage, response: ServerResponse, url: URL, principal: AgentHarnessPrincipal): Promise<void> {
    const path = url.pathname;
    let match = path.match(/^\/v1\/workspaces\/([^/]+)\/agents$/);
    if (match) {
      const workspaceId = uuid(match[1]!, "workspaceId");
      if (request.method === "GET") return sendJson(response, 200, await this.service.listAgents(principal, workspaceId));
      if (request.method === "POST") {
        const body = await jsonBody(request);
        exact(body, ["name", "tags"]);
        return sendJson(response, 201, await this.service.createAgent(principal, workspaceId, idempotency(request), { name: text(body.name, "name"), tags: textArray(body.tags, "tags") }));
      }
      throw methodNotAllowed();
    }
    match = path.match(/^\/v1\/workspaces\/([^/]+)\/agents\/([^/]+)$/);
    if (match) {
      if (request.method !== "GET") throw methodNotAllowed();
      return sendJson(response, 200, await this.service.getAgent(principal, uuid(match[1]!, "workspaceId"), uuid(match[2]!, "agentId")));
    }
    match = path.match(/^\/v1\/workspaces\/([^/]+)\/agents\/([^/]+)\/versions$/);
    if (match) {
      const workspaceId = uuid(match[1]!, "workspaceId"), agentId = uuid(match[2]!, "agentId");
      if (request.method === "GET") return sendJson(response, 200, await this.service.listAgentVersions(principal, workspaceId, agentId));
      if (request.method === "POST") {
        const body = await jsonBody(request);
        exact(body, ["version"]);
        return sendJson(response, 201, await this.service.publishAgentVersion(principal, workspaceId, agentId, idempotency(request), { version: document(body.version, "version") }));
      }
      throw methodNotAllowed();
    }
    match = path.match(/^\/v1\/workspaces\/([^/]+)\/agents\/([^/]+)\/versions\/([^/]+)$/);
    if (match) {
      if (request.method !== "GET") throw methodNotAllowed();
      return sendJson(response, 200, await this.service.getAgentVersion(principal, uuid(match[1]!, "workspaceId"), uuid(match[2]!, "agentId"), uuid(match[3]!, "versionId")));
    }
    match = path.match(/^\/v1\/workspaces\/([^/]+)\/harness\/definitions$/);
    if (match) {
      const workspaceId = uuid(match[1]!, "workspaceId");
      if (request.method === "GET") return sendJson(response, 200, await this.service.listHarnessDefinitions(principal, workspaceId));
      if (request.method === "POST") {
        const body = await jsonBody(request);
        exact(body, ["definition"]);
        return sendJson(response, 201, await this.service.createHarnessDefinition(principal, workspaceId, idempotency(request), { definition: document(body.definition, "definition") }));
      }
      throw methodNotAllowed();
    }
    match = path.match(/^\/v1\/workspaces\/([^/]+)\/harness\/definitions\/([^/]+)$/);
    if (match) {
      if (request.method !== "GET") throw methodNotAllowed();
      return sendJson(response, 200, await this.service.getHarnessDefinition(principal, uuid(match[1]!, "workspaceId"), uuid(match[2]!, "definitionId")));
    }
    match = path.match(/^\/v1\/workspaces\/([^/]+)\/harness\/definitions\/([^/]+)\/versions$/);
    if (match) {
      const workspaceId = uuid(match[1]!, "workspaceId"), definitionId = uuid(match[2]!, "definitionId");
      if (request.method === "GET") return sendJson(response, 200, await this.service.listHarnessVersions(principal, workspaceId, definitionId));
      if (request.method === "POST") {
        const body = await jsonBody(request);
        exact(body, ["version"]);
        return sendJson(response, 201, await this.service.publishHarnessVersion(principal, workspaceId, definitionId, idempotency(request), { version: document(body.version, "version") }));
      }
      throw methodNotAllowed();
    }
    match = path.match(/^\/v1\/workspaces\/([^/]+)\/harness\/definitions\/([^/]+)\/versions\/([^/]+)$/);
    if (match) {
      if (request.method !== "GET") throw methodNotAllowed();
      return sendJson(response, 200, await this.service.getHarnessVersion(principal, uuid(match[1]!, "workspaceId"), uuid(match[2]!, "definitionId"), uuid(match[3]!, "versionId")));
    }
    match = path.match(/^\/v1\/workspaces\/([^/]+)\/harness\/profiles$/);
    if (match) {
      const workspaceId = uuid(match[1]!, "workspaceId");
      if (request.method === "GET") return sendJson(response, 200, await this.service.listHarnessProfiles(principal, workspaceId));
      if (request.method === "POST") {
        const body = await jsonBody(request);
        exact(body, ["profile"]);
        return sendJson(response, 201, await this.service.createHarnessProfile(principal, workspaceId, idempotency(request), { profile: document(body.profile, "profile") }));
      }
      throw methodNotAllowed();
    }
    match = path.match(/^\/v1\/workspaces\/([^/]+)\/harness\/profiles\/([^/]+)$/);
    if (match) {
      if (request.method !== "GET") throw methodNotAllowed();
      return sendJson(response, 200, await this.service.getHarnessProfile(principal, uuid(match[1]!, "workspaceId"), uuid(match[2]!, "profileId")));
    }
    match = path.match(/^\/v1\/workspaces\/([^/]+)\/harness\/profiles\/([^/]+)\/revisions$/);
    if (match) {
      if (request.method !== "POST") throw methodNotAllowed();
      const body = await jsonBody(request);
      exact(body, ["profile", "expectedResourceVersion"]);
      return sendJson(response, 200, await this.service.reviseHarnessProfile(principal, uuid(match[1]!, "workspaceId"), uuid(match[2]!, "profileId"), idempotency(request), { profile: document(body.profile, "profile"), expectedResourceVersion: integer(body.expectedResourceVersion, "expectedResourceVersion") }));
    }
    throw new AgentHarnessHttpError("agent_not_found", "Agent or Harness route was not found");
  }
}

function uuid(value: string, field: string): string { if (!UUID.test(value)) throw new AgentHarnessHttpError("invalid_request", `${field} must be a UUID`); return value; }
function idempotency(request: IncomingMessage): string {
  const values = request.headersDistinct["idempotency-key"] ?? [];
  if (values.length !== 1 || values[0]!.length < 8 || values[0]!.length > 128) throw new AgentHarnessHttpError("invalid_request", "Idempotency-Key is required and must be unique");
  return values[0]!;
}
function exact(body: Record<string, unknown>, required: string[]): void {
  if (required.some((key) => !(key in body)) || Object.keys(body).some((key) => !required.includes(key))) throw new AgentHarnessHttpError("invalid_request", `request body fields are invalid; required: ${required.join(", ")}`);
}
function text(value: unknown, field: string): string { if (typeof value !== "string" || !value || value.length > 96) throw new AgentHarnessHttpError("invalid_request", `${field} is invalid`); return value; }
function textArray(value: unknown, field: string): string[] {
  if (!Array.isArray(value) || value.length > 32 || value.some((item) => typeof item !== "string" || item.length < 1 || item.length > 96)) throw new AgentHarnessHttpError("invalid_request", `${field} is invalid`);
  return value as string[];
}
function document(value: unknown, field: string): JsonDocument {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new AgentHarnessHttpError("invalid_request", `${field} is invalid`);
  return value as JsonDocument;
}
function integer(value: unknown, field: string): number { if (!Number.isSafeInteger(value)) throw new AgentHarnessHttpError("invalid_request", `${field} is invalid`); return value as number; }
function methodNotAllowed(): AgentHarnessHttpError { return new AgentHarnessHttpError("method_not_allowed", "method is not allowed for this route"); }

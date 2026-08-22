import type { IncomingMessage, ServerResponse } from "node:http";
import { jsonBody, requireExactKeys, requiredString, sendJson } from "./http.js";
import type { WorkspaceService } from "./workspace-service.js";
import { WorkspaceHttpError, type WorkspacePrincipal, type WorkspaceRole } from "./workspace-types.js";

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

export class WorkspaceHttpRouter {
  constructor(private readonly service: WorkspaceService) {}

  matches(pathname: string): boolean {
    return pathname === "/v1/workspaces" || pathname === "/v1/workspace-invitations/accept" || pathname.startsWith("/v1/workspaces/");
  }

  async handle(request: IncomingMessage, response: ServerResponse, url: URL, principal: WorkspacePrincipal, reauthorize?: () => Promise<WorkspacePrincipal>): Promise<void> {
    const path = url.pathname;
    if (path === "/v1/workspaces" && request.method === "POST") {
      const body = await jsonBody(request); requireExactKeysOptional(body, ["name"], ["slug"]);
      return sendJson(response, 201, await this.service.createWorkspace(principal, idempotency(request), { name: requiredString(body, "name", 160), ...(typeof body.slug === "string" ? { slug: body.slug } : {}) }));
    }
    if (path === "/v1/workspaces" && request.method === "GET") return sendJson(response, 200, await this.service.listWorkspaces(principal, cursor(url)));
    if (path === "/v1/workspace-invitations/accept" && request.method === "POST") {
      const body = await jsonBody(request); requireExactKeys(body, ["inviteToken"]);
      return sendJson(response, 200, await this.service.acceptInvitation(principal, idempotency(request), requiredString(body, "inviteToken", 512)));
    }

    const workspace = path.match(/^\/v1\/workspaces\/([^/]+)$/);
    if (workspace) {
      const workspaceId = uuid(workspace[1] ?? "", "workspaceId");
      if (request.method === "GET") return sendJson(response, 200, await this.service.getWorkspace(principal, workspaceId));
      if (request.method === "PATCH") {
        const body = await jsonBody(request); requireExactKeys(body, ["name", "expectedVersion"]);
        return sendJson(response, 200, await this.service.updateWorkspace(principal, workspaceId, idempotency(request), { name: requiredString(body, "name", 160), expectedVersion: integer(body.expectedVersion, "expectedVersion") }));
      }
    }

    const invitations = path.match(/^\/v1\/workspaces\/([^/]+)\/invitations$/);
    if (invitations) {
      const workspaceId = uuid(invitations[1] ?? "", "workspaceId");
      if (request.method === "GET") return sendJson(response, 200, await this.service.listInvitations(principal, workspaceId, cursor(url)));
      if (request.method === "POST") {
        const body = await jsonBody(request); requireExactKeys(body, ["role", "expiresIn"]);
        return sendJson(response, 201, await this.service.createInvitation(principal, workspaceId, idempotency(request), { role: role(body.role), expiresIn: integer(body.expiresIn, "expiresIn") }));
      }
    }

    const invitation = path.match(/^\/v1\/workspaces\/([^/]+)\/invitations\/([^/]+)$/);
    if (invitation && request.method === "DELETE") return sendJson(response, 200, await this.service.revokeInvitation(principal, uuid(invitation[1] ?? "", "workspaceId"), uuid(invitation[2] ?? "", "invitationId"), expectedVersion(url), idempotency(request)));

    const members = path.match(/^\/v1\/workspaces\/([^/]+)\/members$/);
    if (members && request.method === "GET") return sendJson(response, 200, await this.service.listMembers(principal, uuid(members[1] ?? "", "workspaceId"), cursor(url)));

    const member = path.match(/^\/v1\/workspaces\/([^/]+)\/members\/([^/]+)$/);
    if (member) {
      const workspaceId = uuid(member[1] ?? "", "workspaceId"); const userId = uuid(member[2] ?? "", "userId");
      if (request.method === "PATCH") {
        const body = await jsonBody(request); requireExactKeys(body, ["role", "expectedVersion"]);
        return sendJson(response, 200, await this.service.updateMember(principal, workspaceId, userId, idempotency(request), { role: role(body.role), expectedVersion: integer(body.expectedVersion, "expectedVersion") }));
      }
      if (request.method === "DELETE") return sendJson(response, 200, await this.service.removeMember(principal, workspaceId, userId, expectedVersion(url), idempotency(request)));
    }

    const ownMembership = path.match(/^\/v1\/workspaces\/([^/]+)\/membership$/);
    if (ownMembership && request.method === "DELETE") return sendJson(response, 200, await this.service.leaveWorkspace(principal, uuid(ownMembership[1] ?? "", "workspaceId"), expectedVersion(url), idempotency(request)));

    const events = path.match(/^\/v1\/workspaces\/([^/]+)\/events$/);
    if (events && request.method === "GET") return this.streamEvents(request, response, principal, uuid(events[1] ?? "", "workspaceId"), reauthorize);
    throw new WorkspaceHttpError("workspace_not_found", "workspace route was not found");
  }

  private async streamEvents(request: IncomingMessage, response: ServerResponse, principal: WorkspacePrincipal, workspaceId: string, reauthorize?: () => Promise<WorkspacePrincipal>): Promise<void> {
    const distinct = request.headersDistinct["last-event-id"] ?? [];
    if (distinct.length > 1 || (distinct[0]?.length ?? 0) > 128) throw new WorkspaceHttpError("invalid_request", "Last-Event-ID is invalid");
    let cursor = distinct[0] ?? "";
    await this.service.eventBatch(principal, workspaceId, cursor);
    response.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-store", connection: "keep-alive" });
    response.write(`event: ready\nid: ${cursor}\ndata: {}\n\n`);
    void (async () => {
      while (!response.destroyed && !response.writableEnded) {
        await new Promise((resolve) => setTimeout(resolve, 1_000));
        if (response.destroyed || response.writableEnded) break;
        try {
          const currentPrincipal = reauthorize ? await reauthorize() : principal;
          const events = await this.service.eventBatch(currentPrincipal, workspaceId, cursor);
          for (const event of events) {
            cursor = event.id;
            response.write(`event: ${event.type}\nid: ${event.id}\ndata: ${JSON.stringify(event.payload)}\n\n`);
          }
          if (events.length === 0) response.write(": heartbeat\n\n");
        } catch (error) {
          if (error instanceof WorkspaceHttpError && (error.code === "workspace_not_found" || error.code === "membership_required")) response.write("event: membership_revoked\ndata: {}\n\n");
          response.end();
          break;
        }
      }
    })();
  }
}

function idempotency(request: IncomingMessage): string {
  const values = request.headersDistinct["idempotency-key"] ?? [];
  if (values.length !== 1 || values[0]!.length < 8 || values[0]!.length > 128) throw new WorkspaceHttpError("invalid_request", "Idempotency-Key is required");
  return values[0]!;
}
function expectedVersion(url: URL): number { return integer(Number(url.searchParams.get("expectedVersion")), "expectedVersion"); }
function cursor(url: URL): string { const value = url.searchParams.get("cursor") ?? ""; if (value.length > 512) throw new WorkspaceHttpError("invalid_request", "cursor is invalid"); return value; }
function uuid(value: string, field: string): string { if (!UUID.test(value)) throw new WorkspaceHttpError("invalid_request", `${field} must be a UUID`); return value; }
function integer(value: unknown, field: string): number { if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 1) throw new WorkspaceHttpError("invalid_request", `${field} must be a positive integer`); return value; }
function role(value: unknown): WorkspaceRole { if (typeof value !== "string" || !["owner", "administrator", "operator", "member", "viewer"].includes(value)) throw new WorkspaceHttpError("invalid_request", "role is invalid"); return value as WorkspaceRole; }
function requireExactKeysOptional(body: Record<string, unknown>, required: string[], optional: string[]): void {
  for (const key of required) if (!(key in body)) throw new WorkspaceHttpError("invalid_request", `missing ${key}`);
  if (Object.keys(body).some((key) => !required.includes(key) && !optional.includes(key))) throw new WorkspaceHttpError("invalid_request", "request contains unsupported fields");
}

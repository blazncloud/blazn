import type { IncomingMessage, ServerResponse } from "node:http";
import { jsonBody, sendJson } from "./http.js";
import type { CreateProjectInput, ProjectService, UpdateProjectInput } from "./project-service.js";
import { ProjectHttpError, type ProjectPrincipal, type ProjectProfileStatus, type ProjectStatus, type PutProjectProfileInput } from "./project-types.js";

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

export class ProjectHttpRouter {
  constructor(private readonly service: ProjectService) {}

  matches(pathname: string): boolean {
    return /^\/v1\/workspaces\/[^/]+\/projects(?:\/[^/]+(?:\/profiles\/[^/]+)?)?$/.test(pathname);
  }

  async handle(request: IncomingMessage, response: ServerResponse, url: URL, principal: ProjectPrincipal): Promise<void> {
    const collection = url.pathname.match(/^\/v1\/workspaces\/([^/]+)\/projects$/);
    if (collection) {
      const workspaceId = uuid(collection[1] ?? "", "workspaceId");
      if (request.method === "GET") {
        return sendJson(response, 200, await this.service.listProjects(principal, workspaceId, status(url), cursor(url)));
      }
      if (request.method === "POST") {
        const body = await jsonBody(request);
        exactOptional(body, ["name"], ["slug", "kind", "description"]);
        const input: CreateProjectInput = { name: nonEmpty(body.name, "name", 160) };
        if (body.slug !== undefined) input.slug = nonEmpty(body.slug, "slug", 64);
        if (body.kind !== undefined) input.kind = nonEmpty(body.kind, "kind", 63);
        if (body.description !== undefined) input.description = text(body.description, "description", 4000);
        return sendJson(response, 201, await this.service.createProject(principal, workspaceId, idempotency(request), input));
      }
      throw methodNotAllowed();
    }

    const profile=url.pathname.match(/^\/v1\/workspaces\/([^/]+)\/projects\/([^/]+)\/profiles\/([^/]+)$/);if(profile){const workspaceId=uuid(profile[1]??"","workspaceId"),projectId=uuid(profile[2]??"","projectId"),kind=nonEmpty(profile[3],"profileKind",63);if(request.method==="GET")return sendJson(response,200,await this.service.getProjectProfile(principal,workspaceId,projectId,kind));if(request.method!=="PUT")throw methodNotAllowed();const body=await jsonBody(request);exactOptional(body,["schemaVersion","draftId","artifactId","digest","status","expectedVersion"],[]);const input:PutProjectProfileInput={schemaVersion:nonEmpty(body.schemaVersion,"schemaVersion",128),draftId:uuid(String(body.draftId??""),"draftId"),artifactId:uuid(String(body.artifactId??""),"artifactId"),digest:nonEmpty(body.digest,"digest",71),status:profileStatus(body.status),expectedVersion:nonnegativeInteger(body.expectedVersion,"expectedVersion")};return sendJson(response,200,await this.service.putProjectProfile(principal,workspaceId,projectId,kind,idempotency(request),input));}
    const resource = url.pathname.match(/^\/v1\/workspaces\/([^/]+)\/projects\/([^/]+)$/);
    if (!resource) throw new ProjectHttpError("project_not_found", "project route was not found");
    const workspaceId = uuid(resource[1] ?? "", "workspaceId");
    const projectId = uuid(resource[2] ?? "", "projectId");
    if (request.method === "GET") return sendJson(response, 200, await this.service.getProject(principal, workspaceId, projectId));
    if (request.method !== "PATCH") throw methodNotAllowed();
    const body = await jsonBody(request);
    exactOptional(body, ["expectedVersion"], ["name", "description", "status"]);
    const input: UpdateProjectInput = { expectedVersion: integer(body.expectedVersion, "expectedVersion") };
    if (body.name !== undefined) input.name = nonEmpty(body.name, "name", 160);
    if (body.description !== undefined) input.description = text(body.description, "description", 4000);
    if (body.status !== undefined) input.status = projectStatus(body.status);
    if (input.name === undefined && input.description === undefined && input.status === undefined) throw new ProjectHttpError("invalid_request", "project update must change name, description, or status");
    return sendJson(response, 200, await this.service.updateProject(principal, workspaceId, projectId, idempotency(request), input));
  }
}

function uuid(value: string, field: string): string {
  if (!UUID.test(value)) throw new ProjectHttpError("invalid_request", `${field} must be a UUID`);
  return value;
}

function idempotency(request: IncomingMessage): string {
  const values = request.headersDistinct["idempotency-key"] ?? [];
  if (values.length !== 1 || values[0]!.length < 8 || values[0]!.length > 128) throw new ProjectHttpError("invalid_request", "Idempotency-Key is required and must be unique");
  return values[0]!;
}

function status(url: URL): ProjectStatus | "all" {
  const values = url.searchParams.getAll("status");
  if (values.length > 1) throw new ProjectHttpError("invalid_request", "status must not be repeated");
  const value = values[0] ?? "active";
  if (value !== "active" && value !== "archived" && value !== "all") throw new ProjectHttpError("invalid_request", "status is invalid");
  return value;
}

function cursor(url: URL): string {
  const values = url.searchParams.getAll("cursor");
  if (values.length > 1 || (values[0]?.length ?? 0) > 512) throw new ProjectHttpError("invalid_request", "cursor is invalid");
  return values[0] ?? "";
}

function exactOptional(body: Record<string, unknown>, required: string[], optional: string[]): void {
  const allowed = new Set([...required, ...optional]);
  const actual = Object.keys(body);
  if (required.some((key) => !(key in body)) || actual.some((key) => !allowed.has(key))) throw new ProjectHttpError("invalid_request", `request body fields are invalid; required: ${required.join(", ")}`);
}

function nonEmpty(value: unknown, field: string, max: number): string {
  if (typeof value !== "string" || value.trim() === "" || value.length > max) throw new ProjectHttpError("invalid_request", `${field} is invalid`);
  return value;
}

function text(value: unknown, field: string, max: number): string {
  if (typeof value !== "string" || value.length > max) throw new ProjectHttpError("invalid_request", `${field} is invalid`);
  return value;
}

function integer(value: unknown, field: string): number {
  if (!Number.isSafeInteger(value) || (value as number) < 1) throw new ProjectHttpError("invalid_request", `${field} must be a positive integer`);
  return value as number;
}
function nonnegativeInteger(value:unknown,field:string):number{if(!Number.isSafeInteger(value)||(value as number)<0)throw new ProjectHttpError("invalid_request",`${field} must be a nonnegative integer`);return value as number;}

function projectStatus(value: unknown): ProjectStatus {
  if (value !== "active" && value !== "archived") throw new ProjectHttpError("invalid_request", "status is invalid");
  return value;
}
function profileStatus(value:unknown):ProjectProfileStatus{if(value!=="active"&&value!=="archived")throw new ProjectHttpError("invalid_request","profile status is invalid");return value;}

function methodNotAllowed(): ProjectHttpError {
  return new ProjectHttpError("method_not_allowed", "method is not allowed for this Project route");
}

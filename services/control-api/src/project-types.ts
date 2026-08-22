import type { WorkspacePrincipal, WorkspaceRole } from "./workspace-types.js";

export type ProjectPrincipal = WorkspacePrincipal;
export type ProjectStatus = "active" | "archived";

export interface Project {
  id: string;
  workspaceId: string;
  slug: string;
  kind: string;
  name: string;
  description: string;
  status: ProjectStatus;
  version: number;
  createdBy: string;
  createdAt: string;
  updatedAt: string;
}

export interface ProjectAccess {
  workspaceStatus: "active" | "archived";
  role: WorkspaceRole;
}

export type ProjectErrorCode =
  | "project_not_found"
  | "project_slug_conflict"
  | "workspace_not_found"
  | "membership_required"
  | "permission_denied"
  | "version_conflict"
  | "idempotency_conflict"
  | "invalid_request"
  | "method_not_allowed";

const statusByCode: Record<ProjectErrorCode, number> = {
  project_not_found: 404,
  project_slug_conflict: 409,
  workspace_not_found: 404,
  membership_required: 403,
  permission_denied: 403,
  version_conflict: 409,
  idempotency_conflict: 409,
  invalid_request: 400,
  method_not_allowed: 405,
};

export class ProjectHttpError extends Error {
  readonly status: number;
  constructor(readonly code: ProjectErrorCode, message: string) {
    super(message);
    this.status = statusByCode[code];
  }
}

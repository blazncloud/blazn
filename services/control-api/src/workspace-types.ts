export const workspaceRoles = ["owner", "administrator", "operator", "member", "viewer"] as const;
export type WorkspaceRole = typeof workspaceRoles[number];

export interface WorkspacePrincipal {
  userId: string;
  email: string;
  displayName: string;
}

export interface Workspace {
  id: string;
  slug: string;
  name: string;
  status: "active" | "archived";
  version: number;
  currentUserRole: WorkspaceRole;
  createdAt: string;
  updatedAt: string;
}

export interface Membership {
  workspaceId: string;
  user: { id: string; email: string; displayName: string; status: "active" };
  role: WorkspaceRole;
  status: "active" | "removed";
  version: number;
  joinedAt: string;
  removedAt?: string;
}

export interface Invitation {
  id: string;
  workspaceId: string;
  role: Exclude<WorkspaceRole, "owner">;
  status: "pending" | "accepted" | "revoked" | "expired";
  version: number;
  createdAt: string;
  expiresAt: string;
}

export interface MutationResult {
  status: "revoked" | "removed" | "left";
  workspaceId: string;
  userId?: string;
  invitationId?: string;
  version: number;
}

export type WorkspaceErrorCode =
  | "workspace_not_found"
  | "workspace_slug_conflict"
  | "membership_required"
  | "permission_denied"
  | "version_conflict"
  | "idempotency_conflict"
  | "invitation_invalid"
  | "invitation_expired"
  | "invitation_consumed"
  | "invitation_revoked"
  | "last_owner"
  | "method_not_allowed"
  | "rate_limited"
  | "invalid_request";

const workspaceErrorStatus: Record<WorkspaceErrorCode, number> = {
  workspace_not_found: 404,
  workspace_slug_conflict: 409,
  membership_required: 403,
  permission_denied: 403,
  version_conflict: 409,
  idempotency_conflict: 409,
  invitation_invalid: 400,
  invitation_expired: 410,
  invitation_consumed: 410,
  invitation_revoked: 410,
  last_owner: 409,
  method_not_allowed: 405,
  rate_limited: 429,
  invalid_request: 400,
};

export class WorkspaceHttpError extends Error {
  readonly status: number;
  constructor(readonly code: WorkspaceErrorCode, message: string) {
    super(message);
    this.status = workspaceErrorStatus[code];
  }
}

export type WorkspaceCapability = "read" | "edit" | "invite" | "manage_members" | "operate";

export function roleAllows(role: WorkspaceRole, capability: WorkspaceCapability): boolean {
  if (role === "owner") return true;
  if (role === "administrator") return true;
  if (role === "operator") return capability === "read" || capability === "operate";
  if (role === "member" || role === "viewer") return capability === "read";
  return false;
}

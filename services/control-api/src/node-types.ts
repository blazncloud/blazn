import type { WorkspacePrincipal, WorkspaceRole } from "./workspace-types.js";

export type NodePrincipal = WorkspacePrincipal;
export type NodePlatform = "linux" | "macos";
export type NodeArchitecture = "amd64" | "arm64";
export type NodeOperationType = "pause" | "resume" | "label" | "cordon" | "uncordon" | "rotate_identity" | "repair" | "update" | "drain" | "remove";

export interface KubernetesBinding { clusterId: string; nodeName: string; nodeUid: string; resourceVersion: string }
export interface NodeIdentityView { generation: number; publicKeyFingerprint: string; status: "active" | "rotating" | "revoked" | "expired"; issuedAt: string; expiresAt: string }
export interface NodeView {
  id: string; workspaceId: string; name: string; kind: "personal" | "shared" | "managed";
  platform: NodePlatform; architecture: NodeArchitecture;
  lifecycleState: "pending" | "installing" | "verifying" | "active" | "paused" | "draining" | "offline" | "quarantined" | "removed";
  trustState: "unverified" | "verifying" | "verified" | "rotating" | "revoked";
  agentEligible: boolean; version: number; capabilityVersion: number | null;
  identity: NodeIdentityView | null; kubernetesBinding?: KubernetesBinding | null;
  createdAt: string; updatedAt: string;
}

export interface EnrollmentRecord {
  id: string; workspaceId: string; requestedName: string; mode: "fresh" | "adopt";
  expectedPlatform: NodePlatform; expectedArchitecture: NodeArchitecture | null;
  tokenHash: string; tokenKeyId: "node-enrollment/v1"; idempotencyKey: string;
  createdBy: string; expiresAt: Date; status: "pending" | "exchanged" | "consumed" | "expired" | "revoked";
  machineBinding: string | null; nodePublicKey: string | null; nodePublicKeyFingerprint: string | null;
  consumedByNodeId: string | null; version: number;
}

export interface NodeOperationView {
  id: string; nodeId: string; type: NodeOperationType;
  status: "pending" | "running" | "succeeded" | "failed" | "cancelled" | "partial" | "recovery_required";
  expectedNodeVersion: number; result: Record<string, unknown> | null; error: Record<string, unknown> | null;
  receipt: Record<string, unknown> | null; createdAt: string;
}

export interface NodeEvent { id: string; type: string; payload: unknown; createdAt: string }

export type NodeErrorCode = "node_not_found" | "enrollment_not_found" | "enrollment_invalid" | "enrollment_expired" |
  "enrollment_consumed" | "permission_denied" | "membership_required" | "version_conflict" |
  "idempotency_conflict" | "identity_rejected" | "heartbeat_replay" | "heartbeat_skew" |
  "capability_digest_invalid" | "state_conflict" | "invalid_request" | "method_not_allowed" |
  "join_credential_invalid" | "join_credential_consumed" | "rate_limited";

const NODE_ERROR_STATUS: Record<NodeErrorCode, number> = {
  node_not_found: 404, enrollment_not_found: 404, enrollment_invalid: 400, enrollment_expired: 410,
  enrollment_consumed: 410, permission_denied: 403, membership_required: 403, version_conflict: 409,
  idempotency_conflict: 409, identity_rejected: 401, heartbeat_replay: 409, heartbeat_skew: 400,
  capability_digest_invalid: 400, state_conflict: 409, invalid_request: 400, method_not_allowed: 405,
  join_credential_invalid: 400, join_credential_consumed: 410, rate_limited: 429,
};

export class NodeHttpError extends Error {
  readonly status: number;
  constructor(readonly code: NodeErrorCode, message: string) { super(message); this.status = NODE_ERROR_STATUS[code]; }
}

export function nodeRoleAllows(role: WorkspaceRole, mutation: boolean): boolean {
  if (role === "owner" || role === "administrator") return true;
  if (role === "operator") return true;
  return !mutation && (role === "member" || role === "viewer");
}

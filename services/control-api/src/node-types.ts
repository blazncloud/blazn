import type { WorkspacePrincipal, WorkspaceRole } from "./workspace-types.js";

export type NodePrincipal = WorkspacePrincipal;
export type NodePlatform = "linux" | "macos";
export type NodeArchitecture = "amd64" | "arm64";
export type NodeOperationType = "pause" | "resume" | "label" | "cordon" | "uncordon" | "rotate_identity" | "repair" | "update" | "drain" | "remove";

export interface KubernetesBinding { clusterId: string; nodeName: string; nodeUid: string; resourceVersion: string }
export interface NodePlanSigningKey { keyId: string; publicKey: string; fingerprint: string }
export interface NodeEnrollmentIdentity { generation: number; signingKeyId: string; publicKeyFingerprint: string; issuedAt: string; expiresAt: string }
export interface ExchangeNodeEnrollmentResponse { plan: Record<string, unknown>; identity: NodeEnrollmentIdentity }
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
  planSigningKey: NodePlanSigningKey;
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

export const NODE_ERROR_STATUS = {
  access_expired: 401, authorization_capacity: 503, authorization_not_found: 404, authorization_pending: 428,
  capability_digest_invalid: 400, device_not_found: 404, device_proof_invalid: 403, device_revoked: 401,
  enrollment_consumed: 410, enrollment_expired: 410, enrollment_invalid: 400, enrollment_not_found: 404,
  expired_token: 400, forwarded_identity_invalid: 400, heartbeat_replay: 409, heartbeat_skew: 400,
  identity_rejected: 403, idempotency_conflict: 409, internal_error: 500, invalid_json: 400,
  invalid_public_key: 400, invalid_request: 400, join_credential_consumed: 410, join_credential_invalid: 400,
  membership_required: 403, method_not_allowed: 405, node_not_found: 404, not_found: 404,
  object_storage_unavailable: 503, permission_denied: 403, proxy_auth_invalid: 403, rate_limited: 429,
  request_too_large: 413, session_revoked: 401, slow_down: 429, state_conflict: 409,
  unauthorized: 401, version_conflict: 409,
};
export type NodeErrorCode = keyof typeof NODE_ERROR_STATUS;

export class NodeHttpError extends Error {
  readonly status: number;
  constructor(readonly code: NodeErrorCode, message: string) { super(message); this.status = NODE_ERROR_STATUS[code]; }
}

export function nodeErrorBody(error: NodeHttpError, requestId: string): { code: NodeErrorCode; message: string; requestId: string } {
  return { code: error.code, message: error.message, requestId };
}

export function nodeRoleAllows(role: WorkspaceRole, mutation: boolean): boolean {
  if (role === "owner" || role === "administrator") return true;
  if (role === "operator") return true;
  return !mutation && (role === "member" || role === "viewer");
}

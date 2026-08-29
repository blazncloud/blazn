import type { WorkspacePrincipal, WorkspaceRole } from "./workspace-types.js";

export type AgentHarnessPrincipal = WorkspacePrincipal;
export type AgentStatus = "active" | "inactive" | "archived";
export type HarnessDefinitionStatus = "approved" | "deprecated" | "prohibited";
export type HarnessProfileStatus = "approved" | "disabled";
export type HarnessKind = "hermes" | "codex-cli" | "claude-code" | "generic-cli";
export type JsonDocument = Record<string, unknown>;

export interface Agent {
  id: string; workspaceId: string; ownerId: string; name: string; tags: string[];
  status: AgentStatus; currentVersionId: string | null; version: number;
  createdAt: string; updatedAt: string;
}
export interface AgentVersion {
  id: string; agentId: string; workspaceId: string; version: number; digest: string;
  document: JsonDocument; createdBy: string; createdAt: string;
}
export interface HarnessDefinition {
  id: string; workspaceId: string; kind: HarnessKind; status: HarnessDefinitionStatus;
  resourceVersion: number; document: JsonDocument; createdAt: string; updatedAt: string;
}
export interface HarnessVersion {
  id: string; definitionId: string; workspaceId: string; version: string; digest: string;
  document: JsonDocument; createdBy: string; createdAt: string;
}
export interface HarnessProfile {
  id: string; workspaceId: string; name: string; harnessVersionId: string;
  status: HarnessProfileStatus; resourceVersion: number; digest: string;
  document: JsonDocument; createdAt: string; updatedAt: string;
}
export interface AgentHarnessAccess { workspaceStatus: "active" | "archived"; role: WorkspaceRole }

export interface CreateAgentInput { name: string; tags: string[] }
export interface PublishAgentVersionInput { version: JsonDocument }
export interface CreateHarnessDefinitionInput { definition: JsonDocument }
export interface PublishHarnessVersionInput { version: JsonDocument }
export interface CreateHarnessProfileInput { profile: JsonDocument }
export interface ReviseHarnessProfileInput { profile: JsonDocument; expectedResourceVersion: number }

export type AgentHarnessErrorCode =
  | "workspace_not_found"
  | "membership_required"
  | "permission_denied"
  | "agent_not_found"
  | "agent_name_conflict"
  | "agent_version_not_found"
  | "agent_version_sequence_conflict"
  | "definition_not_found"
  | "definition_kind_conflict"
  | "harness_version_not_found"
  | "harness_version_conflict"
  | "profile_not_found"
  | "profile_name_conflict"
  | "profile_revision_conflict"
  | "template_version_not_found"
  | "contract_violation"
  | "identity_conflict"
  | "version_conflict"
  | "idempotency_conflict"
  | "invalid_request"
  | "method_not_allowed";

const statuses: Record<AgentHarnessErrorCode, number> = {
  workspace_not_found: 404,
  membership_required: 403,
  permission_denied: 403,
  agent_not_found: 404,
  agent_name_conflict: 409,
  agent_version_not_found: 404,
  agent_version_sequence_conflict: 409,
  definition_not_found: 404,
  definition_kind_conflict: 409,
  harness_version_not_found: 404,
  harness_version_conflict: 409,
  profile_not_found: 404,
  profile_name_conflict: 409,
  profile_revision_conflict: 409,
  template_version_not_found: 404,
  contract_violation: 422,
  identity_conflict: 409,
  version_conflict: 409,
  idempotency_conflict: 409,
  invalid_request: 400,
  method_not_allowed: 405,
};

export class AgentHarnessHttpError extends Error {
  readonly status: number;
  readonly violations: string[];
  constructor(readonly code: AgentHarnessErrorCode, message: string, violations: string[] = []) {
    super(message);
    this.status = statuses[code];
    this.violations = violations.slice(0, 32);
  }
}

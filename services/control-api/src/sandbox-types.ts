import type { WorkspacePrincipal, WorkspaceRole } from "./workspace-types.js";

export type SandboxPrincipal = WorkspacePrincipal & { sessionId: string };
export type SandboxArchitecture = "amd64" | "arm64";
export type SandboxAllocationMode = "direct" | "claim";
export type SandboxState = "requested" | "queued" | "provisioning" | "ready" | "running" | "stopping" | "stopped" | "deleting" | "deleted" | "failed";
export type SandboxOperationType = "create" | "stop" | "delete";
export type SandboxOperationStatus = "pending" | "running" | "succeeded" | "failed" | "recovery_required";
export type SandboxGrantKind = "exec" | "upload" | "download";

export interface SandboxTemplateManifest {
  apiVersion: "blazn.dev/v1alpha1";
  kind: "SandboxTemplate";
  metadata: { name: string };
  spec: SandboxTemplateSpec;
}
export interface SandboxTemplateSpec {
  version: string; description: string; policyProfile: "poc-restricted-v1";
  isolation: "approved-non-sensitive-poc"; expiresInSeconds: number;
  networkProfile: "default-deny-v1"; variants: SandboxTemplateVariant[];
  repositories?: SandboxTemplateRepository[]; artifacts?: SandboxTemplateArtifact[];
}
export interface SandboxTemplateVariant { name:string; platform:"linux"; architecture:SandboxArchitecture; imageIndex:string; imageDigest:string; command:string[]; resources:{requests:ResourceSet;limits:ResourceSet}; placementProfile:"poc-linux-amd64-v1"|"poc-mac-arm64-v1" }
export interface ResourceSet { cpu:string; memory:string; ephemeralStorage:string }
export interface SandboxTemplateRepository { name:string; url:string; destination:string; writable:boolean }
export interface SandboxTemplateArtifact { name:string; path:string; mediaType:string; required:boolean }

export interface SandboxTemplate { id:string; workspaceId:string; name:string; draftVersion:number; draftManifest?:SandboxTemplateManifest; draftDigest?:string; publishedVersionId:string|null; createdAt:string; updatedAt:string }
export interface SandboxTemplateVersion { id:string; workspaceId:string; templateId:string; name:string; version:string; contentDigest:string; manifest:SandboxTemplateManifest; status:"published"|"deprecated"|"prohibited"; createdAt:string }
export interface SandboxSource { repository:string; commit:string }
export interface SandboxSourceBinding extends SandboxSource { url:string; destination:string; writable:boolean }
export interface SandboxArtifactContractEntry { name:string; path:string; mediaType:string; required:boolean }
export interface SandboxCondition { type:string; status:"true"|"false"|"unknown"; reason:string; message?:string; observedAt:string }
export interface SandboxView {
  id:string; workspaceId:string; requestedBy:string; templateId:string; templateVersionId:string;
  templateName:string; templateVersion:string; templateDigest:string; variantName:string;
  imageIndexDigest:string; imageDigest:string; architecture:SandboxArchitecture; allocationMode:SandboxAllocationMode;
  sourceBindings:SandboxSourceBinding[]; artifactContract:{digest:string;items:SandboxArtifactContractEntry[]};
  state:SandboxState; desiredState:"ready"|"stopped"|"deleted"; version:number; queueName:string;
  admissionId:string|null; isolation:"approved-non-sensitive-poc"; expiresAt:string; conditions:SandboxCondition[];
  createdAt:string; updatedAt:string; stoppedAt?:string|null; deletedAt?:string|null;
}
export interface SandboxReceipt { id:string; operationId:string; operationType:SandboxOperationType; status:Exclude<SandboxOperationStatus,"pending"|"running">; cleanupComplete:boolean; artifactExportComplete:boolean; grantsRevoked:boolean; backendDestroyed:boolean; backend:{present:boolean;uid:string|null;resourceVersion:string|null}; result:{artifactIds:string[];warnings:string[]}|null; error:{code:string;message:string;requestId:string}|null; createdAt:string }
export interface SandboxOperation { id:string; sandboxId:string; type:SandboxOperationType; status:SandboxOperationStatus; expectedSandboxVersion:number; receipt:SandboxReceipt|null; createdAt:string; completedAt:string|null }
export interface SandboxEvent { eventId:string; sandboxId:string; operationId:string|null; sequence:number; type:string; payload:Record<string,unknown>; createdAt:string }
export interface SandboxAccessGrant { id:string; sandboxId:string; workspaceId:string; scope:`sandbox.${SandboxGrantKind}`; kind:SandboxGrantKind; state:"active"|"consumed"|"expired"|"revoked"; expiresAt:string; createdAt:string }
export interface SandboxArtifact { id:string; workspaceId:string; sandboxId:string; name:string; path:string; mediaType:string; size:number; sha256:string; exportedAt:string; download:{endpoint:string;size:number;sha256:string;mediaType:string} }

export type SandboxErrorCode = "template_not_found"|"template_name_conflict"|"template_version_not_found"|"template_version_conflict"|"template_invalid"|"template_policy_denied"|"sandbox_not_found"|"sandbox_operation_not_found"|"sandbox_artifact_not_found"|"sandbox_state_conflict"|"sandbox_template_unavailable"|"sandbox_architecture_unavailable"|"sandbox_access_denied"|"access_grant_expired"|"access_grant_consumed"|"access_grant_revoked"|"sandbox_backend_unavailable"|"sandbox_cleanup_incomplete"|"access_expired"|"membership_required"|"permission_denied"|"version_conflict"|"idempotency_conflict"|"rate_limited"|"unauthorized"|"session_revoked"|"invalid_json"|"invalid_request"|"request_too_large"|"internal_error"|"method_not_allowed";
const status:Record<SandboxErrorCode,number>={template_not_found:404,template_name_conflict:409,template_version_not_found:404,template_version_conflict:409,template_invalid:400,template_policy_denied:403,sandbox_not_found:404,sandbox_operation_not_found:404,sandbox_artifact_not_found:404,sandbox_state_conflict:409,sandbox_template_unavailable:409,sandbox_architecture_unavailable:409,sandbox_access_denied:404,access_grant_expired:410,access_grant_consumed:410,access_grant_revoked:410,sandbox_backend_unavailable:503,sandbox_cleanup_incomplete:409,access_expired:401,membership_required:403,permission_denied:403,version_conflict:409,idempotency_conflict:409,rate_limited:429,unauthorized:401,session_revoked:401,invalid_json:400,invalid_request:400,request_too_large:413,internal_error:500,method_not_allowed:405};
export class SandboxHttpError extends Error { readonly status:number; constructor(readonly code:SandboxErrorCode,message:string){super(message);this.status=status[code];} }

export function sandboxRoleAllows(role:WorkspaceRole, capability:"read_template"|"edit_template"|"operate"):boolean {
  if(capability==="read_template") return true;
  if(capability==="edit_template") return role==="owner"||role==="administrator";
  return role==="owner"||role==="administrator"||role==="operator";
}

import type { WorkspacePrincipal, WorkspaceRole } from "./workspace-types.js";

export type DevelopmentPrincipal = WorkspacePrincipal;
export type DevelopmentBuildStatus = "queued"|"building"|"testing"|"succeeded"|"failed"|"cancelled";

export interface DevelopmentProjectRecord {
  workspaceId:string;projectId:string;version:number;manifest:Record<string,unknown>;manifestDigest:string;
  createdBy:string;createdAt:string;updatedAt:string;
}
export interface DevelopmentBuildRecord {
  schemaVersion:"blazn.dev/build-status/v1alpha1";id:string;workspaceId:string;projectId:string;runId:string;
  version:number;status:DevelopmentBuildStatus;requestedBy:string;source:{repository:string;commit:string};
  projectVersion:number;projectManifestDigest:string;template:{versionId:string;digest:string};
  publicationTarget:{templateId:string;candidateVersionId:string;expectedDraftVersion:number;candidateDigest:string};registryRepository:string;
  planDigest:string;publication:{eligible:boolean;refusalReasons:string[];published:null};
  receiptDigest:string|null;evidenceAvailable:boolean;createdAt:string;startedAt?:string;completedAt?:string;errorCode?:string;
}
export interface DevelopmentAccess {workspaceStatus:"active"|"archived";role:WorkspaceRole;projectStatus?:"active"|"archived"}

export type DevelopmentErrorCode="development_project_not_found"|"development_build_not_found"|"project_not_found"|"workspace_not_found"|"permission_denied"|"version_conflict"|"idempotency_conflict"|"invalid_request"|"method_not_allowed";
const status:Record<DevelopmentErrorCode,number>={development_project_not_found:404,development_build_not_found:404,project_not_found:404,workspace_not_found:404,permission_denied:403,version_conflict:409,idempotency_conflict:409,invalid_request:400,method_not_allowed:405};
export class DevelopmentHttpError extends Error {readonly status:number;constructor(readonly code:DevelopmentErrorCode,message:string){super(message);this.status=status[code];}}

import type { WorkspacePrincipal, WorkspaceRole } from "./workspace-types.js";

export type RunPrincipal = WorkspacePrincipal;
export type ProofClass = "synthetic" | "local" | "sandbox" | "provider";
export type RunStatus = "queued" | "running" | "succeeded" | "failed" | "cancelled";
export type ArtifactStatus = "pending" | "ready" | "failed" | "deleted";
export type ArtifactMediaType = "image" | "video" | "audio" | "document" | "data" | "other";

export interface RunPlacement { nodeId?: string; sandboxId?: string; modelRouteId?: string }
export interface RunReceipt {
  schemaVersion: "blazn.run/receipt/v1alpha1";
  proofClass: ProofClass;
  outcome: "succeeded" | "failed" | "cancelled";
  planDigest: string;
  artifactIds: string[];
  summary: { steps: number; warnings: string[] };
}
export interface Run {
  id: string; workspaceId: string; projectId: string; kind: string; proofClass: ProofClass;
  status: RunStatus; version: number; planDigest: string; inputArtifactIds: string[];
  outputNames: string[]; requestedBy: string; placement: RunPlacement | null; receipt: RunReceipt | null;
  createdAt: string; startedAt?: string; completedAt?: string; errorCode?: string;
}
export interface Artifact {
  id: string; workspaceId: string; projectId: string; sourceRunId?: string; kind: string;
  mediaType: ArtifactMediaType; name: string; status: ArtifactStatus; version: number;
  digest?: string; sizeBytes?: number; createdBy: string; createdAt: string; updatedAt: string;
  downloadAvailable: boolean;
}
export interface SyntheticRunProgressInput { sequence:number; phase:string; percent:number; message?:string }
export interface SyntheticRunProgressAck { runId:string; sequence:number; runVersion:number; status:"running" }
export interface CompleteSyntheticRunInput { expectedVersion:number; planDigest:string; artifactIds:string[]; summary:{steps:number;warnings:string[]} }
export interface SyntheticArtifactUploadMetadata { name:string;kind:string;mediaType:ArtifactMediaType;sizeBytes:number;digest:string }
export interface RunAccess { workspaceStatus: "active" | "archived"; role: WorkspaceRole; projectStatus?: "active" | "archived" }

export type RunErrorCode = "run_not_found" | "run_terminal" | "run_sequence_conflict" | "artifact_not_found" | "artifact_name_conflict" | "artifact_digest_mismatch" | "artifact_size_mismatch" | "upload_too_large" | "project_not_found" | "workspace_not_found" | "membership_required" | "permission_denied" | "version_conflict" | "idempotency_conflict" | "invalid_request" | "method_not_allowed";
const statuses: Record<RunErrorCode, number> = { run_not_found:404,run_terminal:409,run_sequence_conflict:409,artifact_not_found:404,artifact_name_conflict:409,artifact_digest_mismatch:400,artifact_size_mismatch:400,upload_too_large:413,project_not_found:404,workspace_not_found:404,membership_required:403,permission_denied:403,version_conflict:409,idempotency_conflict:409,invalid_request:400,method_not_allowed:405 };
export class RunHttpError extends Error { readonly status:number; constructor(readonly code:RunErrorCode,message:string){super(message);this.status=statuses[code];} }

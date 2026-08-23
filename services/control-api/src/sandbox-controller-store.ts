import type { QueryResultRow } from "pg";
import type { Database } from "./db.js";
import type { SandboxAllocationMode, SandboxArchitecture, SandboxOperationStatus, SandboxOperationType } from "./sandbox-types.js";

export interface SandboxControllerSource {
  name: string;
  url: string;
  destination: string;
  writable: boolean;
  commit: string;
}

export interface SandboxControllerArtifactContractEntry {
  name: string;
  path: string;
  mediaType: string;
  required: boolean;
}

export interface SandboxControllerAdmissionIdentity {
  apiVersion: "kueue.x-k8s.io/v1beta1";
  namespace: "blazn-poc-sandboxes";
  name: string;
  uid: string;
  resourceVersion: string;
  clusterQueue: string;
  owner: { apiVersion: "agents.x-k8s.io/v1beta1"; kind: "Sandbox"; name: string; uid: string; controller: true };
  workspaceId: string;
  sandboxId: string;
  admitted: true;
  condition: { type: "Admitted"; status: "True" };
  digest: string;
}

export interface SandboxControllerWorkItem {
  operationId: string;
  workspaceId: string;
  sandboxId: string;
  requestedBy: string;
  operationType: SandboxOperationType;
  expectedSandboxVersion: number;
  leaseToken: string;
  leaseExpiresAt: string;
  attempt: number;
  allocationMode: SandboxAllocationMode;
  desiredState: "ready" | "stopped" | "deleted";
  architecture: SandboxArchitecture;
  templateVersionId: string;
  templateDigest: string;
  variantName: string;
  imageIndexDigest: string;
  imageDigest: string;
  placementProfile: "poc-linux-amd64-v1" | "poc-mac-arm64-v1";
  command: string[];
  resources: {
    requests: { cpu: string; memory: string; ephemeralStorage: string };
    limits: { cpu: string; memory: string; ephemeralStorage: string };
  };
  queueName: string;
  admissionId: string | null;
  backendUid: string | null;
  backendResourceVersion: string | null;
  expiresAt: string;
  sources: SandboxControllerSource[];
  artifacts: SandboxControllerArtifactContractEntry[];
  admission: SandboxControllerAdmissionIdentity | null;
}

export interface SandboxControllerCompletion {
  status: Exclude<SandboxOperationStatus, "pending" | "running">;
  expectedBackendUid: string | null;
  expectedBackendResourceVersion: string | null;
  expectedAdmissionDigest: string | null;
  cleanupComplete: boolean;
  artifactExportComplete: boolean;
  grantsRevoked: boolean;
  backendDestroyed: boolean;
  artifactIds: string[];
  warningCodes: string[];
  error: { code: string; message: string; requestId: string } | null;
}

export type SandboxControllerRetryResult = "retry_scheduled" | "recovery_required" | "fenced";

export class PgSandboxControllerStore {
  constructor(private readonly database: Database) {}

  async health(): Promise<void> {
    await this.database.query("SELECT 1");
  }

  async claim(workerId: string, leaseSeconds: number): Promise<SandboxControllerWorkItem | undefined> {
    const result = await this.database.query("SELECT * FROM sandbox_controller_claim_v2($1,$2)", [workerId, leaseSeconds]);
    return result.rows[0] ? workItemRow(result.rows[0]) : undefined;
  }

  async renew(operationId: string, workerId: string, leaseToken: string, leaseSeconds: number): Promise<string | undefined> {
    const result = await this.database.query<{ renewed_until: Date | string | null }>(
      "SELECT sandbox_controller_renew($1,$2,$3,$4) AS renewed_until",
      [operationId, workerId, leaseToken, leaseSeconds],
    );
    const value = result.rows[0]?.renewed_until;
    return value ? timestamp(value) : undefined;
  }

  async bindBackend(operationId: string, workerId: string, leaseToken: string, backend: { uid: string; resourceVersion: string; admission: SandboxControllerAdmissionIdentity }): Promise<boolean> {
    const admission = backend.admission;
    const result = await this.database.query<{ bound: boolean }>(
      "SELECT sandbox_controller_bind_backend_v2($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22) AS bound",
      [operationId, workerId, leaseToken, backend.uid, backend.resourceVersion, admission.apiVersion,
        admission.namespace, admission.name, admission.uid, admission.resourceVersion, admission.clusterQueue,
        admission.owner.apiVersion, admission.owner.kind, admission.owner.name, admission.owner.uid,
        admission.owner.controller, admission.workspaceId, admission.sandboxId, admission.admitted,
        admission.condition.type, admission.condition.status, rawDigest(admission.digest)],
    );
    return result.rows[0]?.bound === true;
  }

  async retry(operationId: string, workerId: string, leaseToken: string, delaySeconds: number, error: { code: string; message: string; requestId: string }): Promise<SandboxControllerRetryResult> {
    const result = await this.database.query<{ outcome: SandboxControllerRetryResult }>(
      "SELECT sandbox_controller_retry($1,$2,$3,$4,$5,$6,$7) AS outcome",
      [operationId, workerId, leaseToken, delaySeconds, error.code, error.message, error.requestId],
    );
    return result.rows[0]!.outcome;
  }

  async complete(operationId: string, workerId: string, leaseToken: string, completion: SandboxControllerCompletion): Promise<boolean> {
    const error = completion.error;
    const result = await this.database.query<{ completed: boolean }>(
      "SELECT sandbox_controller_complete_v2($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::uuid[],$13::text[],$14,$15,$16) AS completed",
      [operationId, workerId, leaseToken, completion.status, completion.expectedBackendUid,
        completion.expectedBackendResourceVersion, completion.expectedAdmissionDigest ? rawDigest(completion.expectedAdmissionDigest) : null, completion.cleanupComplete,
        completion.artifactExportComplete, completion.grantsRevoked, completion.backendDestroyed,
        completion.artifactIds, completion.warningCodes, error?.code ?? null, error?.message ?? null, error?.requestId ?? null],
    );
    return result.rows[0]?.completed === true;
  }

  async enqueueExpired(limit: number): Promise<Array<{ operationId: string; sandboxId: string }>> {
    const result = await this.database.query<{ operation_id: string; sandbox_id: string }>(
      "SELECT * FROM sandbox_controller_enqueue_expired($1)", [limit],
    );
    return result.rows.map((row) => ({ operationId: row.operation_id, sandboxId: row.sandbox_id }));
  }
}

function workItemRow(row: QueryResultRow): SandboxControllerWorkItem {
  const names = requiredStringArray(row.source_names), urls = requiredStringArray(row.source_urls),
    destinations = requiredStringArray(row.source_destinations), writable = requiredBooleanArray(row.source_writable),
    commits = requiredStringArray(row.source_commits);
  if (![urls.length, destinations.length, writable.length, commits.length].every((length) => length === names.length)) {
    throw new Error("sandbox controller source columns are inconsistent");
  }
  const artifactNames = requiredStringArray(row.artifact_names), artifactPaths = requiredStringArray(row.artifact_paths),
    artifactMediaTypes = requiredStringArray(row.artifact_media_types), artifactRequired = requiredBooleanArray(row.artifact_required);
  if (![artifactPaths.length, artifactMediaTypes.length, artifactRequired.length].every((length) => length === artifactNames.length)) {
    throw new Error("sandbox controller artifact columns are inconsistent");
  }
  return {
    operationId: row.operation_id, workspaceId: row.workspace_id, sandboxId: row.sandbox_id,
    requestedBy: row.requested_by,
    operationType: row.operation_type, expectedSandboxVersion: Number(row.expected_sandbox_version),
    leaseToken: row.lease_token, leaseExpiresAt: timestamp(row.lease_expires_at), attempt: Number(row.attempt),
    allocationMode: row.allocation_mode, desiredState: row.desired_state, architecture: row.architecture,
    templateVersionId: row.template_version_id, templateDigest: `sha256:${String(row.template_digest).trim()}`,
    variantName: row.variant_name, imageIndexDigest: row.image_index_digest, imageDigest: row.image_child_digest,
    placementProfile: row.placement_profile, command: requiredStringArray(row.command),
    resources: { requests: { cpu: row.request_cpu, memory: row.request_memory, ephemeralStorage: row.request_ephemeral_storage },
      limits: { cpu: row.limit_cpu, memory: row.limit_memory, ephemeralStorage: row.limit_ephemeral_storage } },
    queueName: row.queue_name, admissionId: row.admission_id, backendUid: row.backend_uid,
    backendResourceVersion: row.backend_resource_version, expiresAt: timestamp(row.expires_at),
    sources: names.map((name, index) => ({ name, url: urls[index]!, destination: destinations[index]!,
      writable: writable[index]!, commit: commits[index]! })),
    artifacts: artifactNames.map((name, index) => ({ name, path: artifactPaths[index]!,
      mediaType: artifactMediaTypes[index]!, required: artifactRequired[index]! })),
    admission: admissionRow(row),
  };
}

function admissionRow(row: QueryResultRow): SandboxControllerAdmissionIdentity | null {
  if (row.admission_digest === null || row.admission_digest === undefined) return null;
  const identity = {
    apiVersion: row.workload_api_version, namespace: row.workload_namespace, name: row.workload_name,
    uid: row.workload_uid, resourceVersion: row.workload_resource_version, clusterQueue: row.admitted_cluster_queue,
    owner: { apiVersion: row.owner_api_version, kind: row.owner_kind, name: row.owner_name,
      uid: row.owner_uid, controller: row.owner_controller },
    workspaceId: row.workspace_label, sandboxId: row.sandbox_label, admitted: row.admitted,
    condition: { type: row.condition_type, status: row.condition_status },
    digest: `sha256:${String(row.admission_digest).trim()}`,
  };
  if (identity.apiVersion !== "kueue.x-k8s.io/v1beta1" || identity.namespace !== "blazn-poc-sandboxes" ||
      identity.owner.apiVersion !== "agents.x-k8s.io/v1beta1" || identity.owner.kind !== "Sandbox" ||
      identity.owner.controller !== true || identity.admitted !== true || identity.condition.type !== "Admitted" ||
      identity.condition.status !== "True" || identity.workspaceId !== row.workspace_id || identity.sandboxId !== row.sandbox_id ||
      identity.owner.name !== row.sandbox_id || identity.owner.uid !== row.backend_uid || identity.uid !== row.admission_id ||
      Object.values(identity).some((value) => value === null || value === undefined)) {
    throw new Error("sandbox controller admission identity is inconsistent");
  }
  rawDigest(identity.digest);
  return identity as SandboxControllerAdmissionIdentity;
}

function rawDigest(value: string): string {
  if (!/^sha256:[0-9a-f]{64}$/.test(value)) throw new Error("sandbox controller admission digest is invalid");
  return value.slice(7);
}

function requiredStringArray(value: unknown): string[] {
  if (!Array.isArray(value) || !value.every((entry) => typeof entry === "string")) throw new Error("sandbox controller string array is invalid");
  return value;
}
function requiredBooleanArray(value: unknown): boolean[] {
  if (!Array.isArray(value) || !value.every((entry) => typeof entry === "boolean")) throw new Error("sandbox controller boolean array is invalid");
  return value;
}
function timestamp(value: Date | string): string {
  return value instanceof Date ? value.toISOString() : new Date(value).toISOString();
}

import type { QueryResultRow } from "pg";
import { createHash } from "node:crypto";
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

export interface SandboxControllerObjectIdentity {
  apiVersion: string;
  kind: string;
  namespace: string;
  name: string;
  uid: string;
  resourceVersion: string;
}

export interface SandboxControllerAdmissionObservation {
  sandbox: SandboxControllerObjectIdentity;
  pod: SandboxControllerObjectIdentity;
  workload: SandboxControllerAdmissionIdentity;
  digest: string;
}

export interface SandboxControllerSourceMaterialization {
  name: string; url: string; destination: string; commit: string; tree: string;
  contentDigest: string; fileCount: number; totalBytes: number; writable: boolean;
}

export interface SandboxControllerSourceReceipt {
  schemaVersion: "blazn.dev/sandbox-source-materialization/v1";
  manifestDigest: string;
  sources: SandboxControllerSourceMaterialization[];
  digest: string;
}

export interface SandboxControllerPersistedArtifact {
  id: string; name: string; path: string; mediaType: string; digest: string; size: number; objectKey: string; exportedAt: string;
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
  persistedWorkloadDigest: string | null;
  admissionObservation: SandboxControllerAdmissionObservation | null;
  sourceMaterialization: SandboxControllerSourceReceipt | null;
  sourceBootstrapObservation: SandboxControllerAdmissionObservation | null;
  persistedArtifacts: SandboxControllerPersistedArtifact[];
}

export interface SandboxControllerCompletion {
  status: Exclude<SandboxOperationStatus, "pending" | "running">;
  expectedBackendUid: string | null;
  expectedBackendResourceVersion: string | null;
  expectedWorkloadDigest: string | null;
  expectedObservationDigest: string | null;
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
    const result = await this.database.query("SELECT * FROM sandbox_controller_claim_v5($1,$2)", [workerId, leaseSeconds]);
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

  async bindBackend(operationId: string, workerId: string, leaseToken: string, observation: SandboxControllerAdmissionObservation): Promise<boolean> {
    validateObservation(observation);
    const admission = observation.workload;
    const result = await this.database.query<{ bound: boolean }>(
      "SELECT sandbox_controller_bind_backend_v4($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35) AS bound",
      [operationId, workerId, leaseToken, observation.sandbox.uid, observation.sandbox.resourceVersion,
        observation.sandbox.apiVersion, observation.sandbox.kind, observation.sandbox.namespace,
        observation.sandbox.name, observation.sandbox.uid, observation.sandbox.resourceVersion,
        observation.pod.apiVersion, observation.pod.kind, observation.pod.namespace,
        observation.pod.name, observation.pod.uid, observation.pod.resourceVersion,
        admission.apiVersion, admission.namespace, admission.name, admission.uid, admission.resourceVersion, admission.clusterQueue,
        admission.owner.apiVersion, admission.owner.kind, admission.owner.name, admission.owner.uid,
        admission.owner.controller, admission.workspaceId, admission.sandboxId, admission.admitted,
        admission.condition.type, admission.condition.status, rawDigest(admission.digest), rawDigest(observation.digest)],
    );
    return result.rows[0]?.bound === true;
  }

  async recordSources(operationId: string, workerId: string, leaseToken: string,
    observation: SandboxControllerAdmissionObservation, receipt: SandboxControllerSourceReceipt): Promise<boolean> {
    validateObservation(observation);
    validateSourceReceipt(receipt);
    const result = await this.database.query<{ recorded: boolean }>(
      "SELECT sandbox_controller_record_source_materialization_v1($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10::jsonb) AS recorded",
      [operationId, workerId, leaseToken, observation.sandbox.uid, observation.sandbox.resourceVersion,
        rawDigest(observation.digest), rawDigest(receipt.manifestDigest), rawDigest(receipt.digest),
        JSON.stringify(receipt), JSON.stringify(observation)],
    );
    return result.rows[0]?.recorded === true;
  }

  async recordArtifact(operationId: string, workerId: string, leaseToken: string,
    observation: SandboxControllerAdmissionObservation, artifact: Omit<SandboxControllerPersistedArtifact, "id" | "exportedAt">): Promise<string | undefined> {
    validateObservation(observation);
    if (!artifactName(artifact.name) || !artifact.path.startsWith("/workspace/artifacts/") ||
      !/^[a-z0-9][a-z0-9.+-]*\/[a-z0-9][a-z0-9.+-]*$/.test(artifact.mediaType) || !digest(artifact.digest) ||
      !Number.isSafeInteger(artifact.size) || artifact.size < 0 || artifact.size > 8 * 1024 * 1024) throw new Error("sandbox artifact record is invalid");
    const result = await this.database.query<{ artifact_id: string; exported_at: Date | string }>(
      "SELECT artifact_id,exported_at FROM sandbox_controller_record_artifact_v1($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)",
      [operationId, workerId, leaseToken, observation.sandbox.uid, observation.sandbox.resourceVersion,
        rawDigest(observation.workload.digest), rawDigest(observation.digest), artifact.name, artifact.path,
        artifact.mediaType, rawDigest(artifact.digest), artifact.size, artifact.objectKey],
    );
    return result.rows[0]?.artifact_id ?? undefined;
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
      "SELECT sandbox_controller_complete_v4($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13::uuid[],$14::text[],$15,$16,$17) AS completed",
      [operationId, workerId, leaseToken, completion.status, completion.expectedBackendUid,
        completion.expectedBackendResourceVersion,
        completion.expectedWorkloadDigest ? rawDigest(completion.expectedWorkloadDigest) : null,
        completion.expectedObservationDigest ? rawDigest(completion.expectedObservationDigest) : null,
        completion.cleanupComplete,
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
    ...observationRow(row),
    ...sourceReceiptRow(row), persistedArtifacts: persistedArtifactRows(row),
  };
}

function persistedArtifactRows(row: QueryResultRow): SandboxControllerPersistedArtifact[] {
  const ids=requiredStringArray(row.exported_artifact_ids),names=requiredStringArray(row.exported_artifact_names),
    paths=requiredStringArray(row.exported_artifact_paths),media=requiredStringArray(row.exported_artifact_media_types),
    digests=requiredStringArray(row.exported_artifact_digests),sizes=requiredNumberArray(row.exported_artifact_sizes),
    keys=requiredStringArray(row.exported_artifact_keys),times=requiredTimestampArray(row.exported_artifact_times);
  if(![names.length,paths.length,media.length,digests.length,sizes.length,keys.length,times.length].every(length=>length===ids.length))throw new Error("sandbox exported artifact columns are inconsistent");
  return ids.map((id,index)=>{const name=names[index]!,path=paths[index]!,mediaType=media[index]!,digestValue=`sha256:${digests[index]!.trim()}`,size=sizes[index]!,objectKey=keys[index]!,exportedAt=times[index]!;
    if(!/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(id)||!artifactName(name)||
      index>0&&names[index-1]!>=name||!path.startsWith("/workspace/artifacts/")||!/^[a-z0-9][a-z0-9.+-]*\/[a-z0-9][a-z0-9.+-]*$/.test(mediaType)||
      !digest(digestValue)||size<0||size>8*1024*1024||objectKey!==`workspaces/${row.workspace_id}/sandboxes/${row.sandbox_id}/artifacts/${name}`)
      throw new Error("sandbox exported artifact identity is inconsistent");
    return{id,name,path,mediaType,digest:digestValue,size,objectKey,exportedAt};});
}

function sourceReceiptRow(row: QueryResultRow): Pick<SandboxControllerWorkItem, "sourceMaterialization" | "sourceBootstrapObservation"> {
  const receipt = row.source_materialization_receipt as SandboxControllerSourceReceipt | null | undefined;
  const observation = row.source_bootstrap_observation as SandboxControllerAdmissionObservation | null | undefined;
  if (receipt != null) validateSourceReceipt(receipt);
  if (observation != null) validateObservation(observation);
  if (receipt == null && observation != null) throw new Error("source bootstrap observation lacks a receipt");
  return { sourceMaterialization: receipt ?? null, sourceBootstrapObservation: observation ?? null };
}

function validateSourceReceipt(receipt: SandboxControllerSourceReceipt): void {
  if (!receipt || receipt.schemaVersion !== "blazn.dev/sandbox-source-materialization/v1" || !digest(receipt.manifestDigest) ||
    !digest(receipt.digest) || !Array.isArray(receipt.sources) || receipt.sources.length > 32) {
    throw new Error("sandbox source materialization receipt is invalid");
  }
  let previous = "";
  for (const source of receipt.sources) {
    if (!source || !/^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(source.name) || source.name <= previous ||
      !/^https:\/\//.test(source.url) || !/^\/workspace\/src\//.test(source.destination) ||
      !/^(?:[0-9a-f]{40}|[0-9a-f]{64})$/.test(source.commit) || !/^(?:[0-9a-f]{40}|[0-9a-f]{64})$/.test(source.tree) ||
      !digest(source.contentDigest) || !Number.isInteger(source.fileCount) || source.fileCount < 0 || source.fileCount > 100000 ||
      !Number.isInteger(source.totalBytes) || source.totalBytes < 0 || source.totalBytes > 256 * 1024 * 1024 || typeof source.writable !== "boolean") {
      throw new Error("sandbox source materialization entry is invalid");
    }
    previous = source.name;
  }
}

function digest(value: unknown): value is string { return typeof value === "string" && /^sha256:[0-9a-f]{64}$/.test(value); }
function artifactName(value: unknown): value is string { return typeof value === "string" && /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(value); }

function observationRow(row: QueryResultRow): Pick<SandboxControllerWorkItem, "persistedWorkloadDigest" | "admissionObservation"> {
  const workloadFields = [row.workload_api_version, row.workload_namespace, row.workload_name,
    row.workload_uid, row.workload_resource_version, row.admitted_cluster_queue, row.owner_api_version,
    row.owner_kind, row.owner_name, row.owner_uid, row.owner_controller, row.workspace_label,
    row.sandbox_label, row.admitted, row.condition_type, row.condition_status];
  const observationFields = [row.pod_api_version, row.pod_kind, row.pod_namespace, row.pod_name,
    row.pod_uid, row.pod_resource_version, row.observation_digest];
  if (row.admission_digest === null || row.admission_digest === undefined) {
    if (workloadFields.some(present) || observationFields.some(present)) throw new Error("sandbox controller admission observation is partially populated");
    return { persistedWorkloadDigest: null, admissionObservation: null };
  }
  if (workloadFields.some((value) => !present(value))) throw new Error("sandbox controller admission identity is incomplete");
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
  const workload = identity as SandboxControllerAdmissionIdentity;
  if (!observationFields.some(present)) return { persistedWorkloadDigest: workload.digest, admissionObservation: null };
  if (observationFields.some((value) => !present(value))) throw new Error("sandbox controller admission observation is incomplete");
  const observation: SandboxControllerAdmissionObservation = {
    sandbox: { apiVersion: "agents.x-k8s.io/v1beta1", kind: "Sandbox", namespace: "blazn-poc-sandboxes",
      name: row.sandbox_id, uid: row.backend_uid, resourceVersion: row.backend_resource_version },
    pod: { apiVersion: row.pod_api_version, kind: row.pod_kind, namespace: row.pod_namespace,
      name: row.pod_name, uid: row.pod_uid, resourceVersion: row.pod_resource_version },
    workload,
    digest: `sha256:${String(row.observation_digest).trim()}`,
  };
  validateObservation(observation);
  return { persistedWorkloadDigest: workload.digest, admissionObservation: observation };
}

function present(value: unknown): boolean { return value !== null && value !== undefined; }

function validateObservation(value: SandboxControllerAdmissionObservation): void {
  const workloadCanonical = ["sandbox-workload-admission-v1", value.workload.apiVersion,
    value.workload.namespace, value.workload.name, value.workload.uid, value.workload.resourceVersion,
    value.workload.clusterQueue, value.workload.owner.apiVersion, value.workload.owner.kind,
    value.workload.owner.name, value.workload.owner.uid, String(value.workload.owner.controller),
    value.workload.workspaceId, value.workload.sandboxId, String(value.workload.admitted),
    value.workload.condition.type, value.workload.condition.status].join("\n");
  const workloadDigest = `sha256:${createHash("sha256").update(workloadCanonical).digest("hex")}`;
  const observationCanonical = ["sandbox-admission-observation-v1", value.sandbox.apiVersion,
    value.sandbox.kind, value.sandbox.namespace, value.sandbox.name, value.sandbox.uid,
    value.sandbox.resourceVersion, value.pod.apiVersion, value.pod.kind, value.pod.namespace,
    value.pod.name, value.pod.uid, value.pod.resourceVersion, value.workload.apiVersion,
    value.workload.namespace, value.workload.name, value.workload.uid, value.workload.resourceVersion,
    value.workload.clusterQueue, value.workload.owner.apiVersion, value.workload.owner.kind,
    value.workload.owner.name, value.workload.owner.uid, String(value.workload.owner.controller),
    value.workload.workspaceId, value.workload.sandboxId, String(value.workload.admitted),
    value.workload.condition.type, value.workload.condition.status, value.workload.digest].join("\n");
  const observationDigest = `sha256:${createHash("sha256").update(observationCanonical).digest("hex")}`;
  if (value.sandbox.apiVersion !== "agents.x-k8s.io/v1beta1" || value.sandbox.kind !== "Sandbox" ||
      value.sandbox.namespace !== "blazn-poc-sandboxes" || value.pod.apiVersion !== "v1" ||
      value.pod.kind !== "Pod" || value.pod.namespace !== "blazn-poc-sandboxes" ||
      value.sandbox.name !== value.workload.sandboxId || value.sandbox.name !== value.workload.owner.name ||
      value.sandbox.uid !== value.workload.owner.uid || workloadDigest !== value.workload.digest ||
      observationDigest !== value.digest) throw new Error("sandbox controller admission observation is inconsistent");
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
function requiredNumberArray(value: unknown): number[] {
  if (!Array.isArray(value) || !value.every((entry) => Number.isSafeInteger(Number(entry)))) throw new Error("sandbox controller number array is invalid");
  return value.map(Number);
}
function requiredTimestampArray(value: unknown): string[] {
  if (!Array.isArray(value)) throw new Error("sandbox controller timestamp array is invalid");
  return value.map((entry) => timestamp(entry as Date | string));
}
function timestamp(value: Date | string): string {
  return value instanceof Date ? value.toISOString() : new Date(value).toISOString();
}

import { readFile } from "node:fs/promises";
import type { EnrollmentRecord, NodeArchitecture } from "./node-types.js";
import type { NodePlanSigner } from "./node-crypto.js";

export interface NodePlanContext {
  planId: string; nodeId: string; enrollment: EnrollmentRecord; architecture: NodeArchitecture;
  machineFingerprint: string; nodePublicKeyFingerprint: string; issuedAt: Date; expiresAt: Date;
}

export interface NodePlanFactory { create(context: NodePlanContext): Promise<Record<string, unknown>> }

export class TemplateNodePlanFactory implements NodePlanFactory {
  constructor(private readonly templateFile: string, private readonly signer: NodePlanSigner) {}

  async create(context: NodePlanContext): Promise<Record<string, unknown>> {
    const parsed: unknown = JSON.parse(await readFile(this.templateFile, "utf8"));
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error("node install plan template must be an object");
    const template = parsed as Record<string, unknown>;
    for (const forbidden of ["planId", "nodeId", "enrollmentId", "workspaceId", "idempotencyKey", "approvedBy", "approvedAt", "hostname", "mode", "installProfile", "target", "issuedAt", "expiresAt", "signingKeyId", "digest", "signature"]) {
      if (forbidden in template) throw new Error(`node install plan template must not set ${forbidden}`);
    }
    const profile = context.enrollment.mode === "fresh" ? "ubuntu-26.04-amd64-worker/v1"
      : context.enrollment.expectedPlatform === "macos" ? "macos-lima-worker-adopt/v1" : "existing-linux-worker-adopt/v1";
    if (profile === "ubuntu-26.04-amd64-worker/v1" && context.architecture !== "amd64") throw new Error("fresh POC install profile requires amd64");
    const unsigned = {
      ...template,
      schemaVersion: "nodes/v1alpha1",
      planId: context.planId,
      nodeId: context.nodeId,
      enrollmentId: context.enrollment.id,
      workspaceId: context.enrollment.workspaceId,
      idempotencyKey: context.enrollment.idempotencyKey,
      approvedBy: context.enrollment.createdBy,
      approvedAt: context.issuedAt.toISOString(),
      hostname: context.enrollment.requestedName,
      mode: context.enrollment.mode,
      installProfile: profile,
      target: {
        platform: context.enrollment.expectedPlatform,
        architecture: context.architecture,
        machineFingerprint: context.machineFingerprint,
        nodePublicKeyFingerprint: `sha256:${context.nodePublicKeyFingerprint}`,
        minCpu: 1,
        minMemoryBytes: 1_073_741_824,
        minDiskBytes: 10_737_418_240,
      },
      issuedAt: context.issuedAt.toISOString(),
      expiresAt: context.expiresAt.toISOString(),
    };
    const signed = await this.signer.sign(unsigned);
    validateCompletePlan(signed);
    return signed;
  }
}

const PLAN_KEYS=["schemaVersion","planId","nodeId","enrollmentId","workspaceId","idempotencyKey","approvedBy","approvedAt","hostname","mode","installProfile","cluster","target","registryTrust","components","nodeService","labels","taints","resourceBounds","mutations","validationTests","rollback","issuedAt","expiresAt","signingKeyId","digest","signature"];
function validateCompletePlan(plan:Record<string,unknown>):void{
  if(Object.keys(plan).length!==PLAN_KEYS.length||PLAN_KEYS.some(key=>!(key in plan))||Object.keys(plan).some(key=>!PLAN_KEYS.includes(key)))throw new Error("signed node install plan does not match the frozen top-level schema");
  for(const field of ["cluster","target","nodeService","resourceBounds","rollback"]){if(!plan[field]||typeof plan[field]!=="object"||Array.isArray(plan[field]))throw new Error(`signed node install plan ${field} is invalid`);}
  for(const field of ["registryTrust","components","taints","mutations","validationTests"]){if(!Array.isArray(plan[field]))throw new Error(`signed node install plan ${field} is invalid`);}
  if(!plan.labels||typeof plan.labels!=="object"||Array.isArray(plan.labels))throw new Error("signed node install plan labels are invalid");
  const cluster=plan.cluster as Record<string,unknown>;
  if(cluster.workerOnly!==true||cluster.joinCredentialEndpoint!=="/v1/node-service/join-credentials"||typeof cluster.id!=="string"||!cluster.id)throw new Error("signed node install plan cluster is not worker-only");
  if(typeof plan.digest!=="string"||!/^sha256:[0-9a-f]{64}$/.test(plan.digest)||typeof plan.signature!=="string"||!/^[A-Za-z0-9_-]{86}$/.test(plan.signature))throw new Error("signed node install plan proof is invalid");
}

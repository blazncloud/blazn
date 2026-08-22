import { readFile } from "node:fs/promises";
import type { EnrollmentRecord, NodeArchitecture, NodePlanSigningKey } from "./node-types.js";
import type { NodePlanSigner } from "./node-crypto.js";
import { NODE_INSTALL_PROFILES, type NodeInstallProfile, validateSignedNodeInstallPlan } from "./node-plan-validator.js";

export interface NodePlanContext {
  planId: string; nodeId: string; enrollment: EnrollmentRecord; architecture: NodeArchitecture;
  machineFingerprint: string; nodePublicKeyFingerprint: string; issuedAt: Date; expiresAt: Date;
}

export interface NodePlanFactory {
  signingKey(): Promise<NodePlanSigningKey>;
  create(context: NodePlanContext): Promise<Record<string, unknown>>;
}

export class TemplateNodePlanFactory implements NodePlanFactory {
  constructor(private readonly templateFile: string, private readonly signer: NodePlanSigner) {}

  signingKey(): Promise<NodePlanSigningKey> { return this.signer.publicKey(); }

  async create(context: NodePlanContext): Promise<Record<string, unknown>> {
    const parsed: unknown = JSON.parse(await readFile(this.templateFile, "utf8"));
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error("node install plan template must be an object");
    const bundle = parsed as Record<string, unknown>;
    exactKeys(bundle,["schemaVersion","templateId","profiles"],"template bundle");
    if(bundle.schemaVersion!=="blazn.dev/node-install-plan-templates/v1"||bundle.templateId!=="frontro-poc-worker/v1")throw new Error("node install plan template bundle identity is invalid");
    if(!bundle.profiles||typeof bundle.profiles!=="object"||Array.isArray(bundle.profiles))throw new Error("node install plan template profiles must be an object");
    const profiles=bundle.profiles as Record<string,unknown>;exactKeys(profiles,[...NODE_INSTALL_PROFILES],"template profiles");
    const profile = (context.enrollment.mode === "fresh" ? "ubuntu-26.04-amd64-worker/v1"
      : context.enrollment.expectedPlatform === "macos" ? "macos-lima-worker-adopt/v1" : "existing-linux-worker-adopt/v1") as NodeInstallProfile;
    if (profile === "ubuntu-26.04-amd64-worker/v1" && context.architecture !== "amd64") throw new Error("fresh POC install profile requires amd64");
    const selected=profiles[profile];if(!selected||typeof selected!=="object"||Array.isArray(selected))throw new Error(`node install plan profile ${profile} is invalid`);
    const template=selected as Record<string,unknown>;exactKeys(template,["cluster","registryTrust","components","nodeService","labels","taints","resourceBounds","mutations","validationTests","rollback"],`template profile ${profile}`);
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
    validateSignedNodeInstallPlan(signed,profile,this.signer.keyId);
    return signed;
  }
}
function exactKeys(value:Record<string,unknown>,keys:string[],name:string){if(Object.keys(value).length!==keys.length||keys.some(k=>!(k in value))||Object.keys(value).some(k=>!keys.includes(k)))throw new Error(`${name} has unsupported or missing fields`);}

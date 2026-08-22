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
    return this.signer.sign(unsigned);
  }
}

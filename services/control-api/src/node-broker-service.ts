import { randomUUID } from "node:crypto";
import {
  brokerRequestDigest,
  credentialHash,
  openJoinCredential,
  sealJoinCredential,
} from "./node-broker-crypto.js";
import { verifyNodePlanSignature, verifyNodeProof } from "./node-crypto.js";
import {
  NODE_INSTALL_PROFILES,
  validateSignedNodeInstallPlan,
  type NodeInstallProfile,
} from "./node-plan-validator.js";
import type {
  BrokerIssuanceIntent,
  NodeBrokerStore,
} from "./node-broker-store.js";
import type {
  BrokerBinding,
  IssuedWorkerCredential,
  JoinCredentialRequest,
  JoinCredentialResponse,
  StoredJoinIssuance,
  WorkerCredentialIssuer,
} from "./node-broker-types.js";
import { NodeHttpError } from "./node-types.js";

export class NodeBrokerService {
  constructor(
    private readonly store: NodeBrokerStore,
    private readonly joinKey: () => Promise<Buffer>,
    private readonly issuer: WorkerCredentialIssuer,
    private readonly providerTimeoutMs = 10_000,
  ) {}

  async issue(
    idempotencyKey: string,
    request: JoinCredentialRequest,
    proof: string,
  ): Promise<JoinCredentialResponse> {
    validateRequest(idempotencyKey, request, proof);
    const digest = brokerRequestDigest(request),
      key = await this.joinKey();
    if (key.length !== 32)
      throw new Error("join credential key must be exactly 32 raw bytes");
    const prepared = await this.store.transaction(async (tx) => {
      await tx.lockNode(request.nodeId);
      if (!(await tx.lockBinding(request)))
        throw invalidCredential("join credential bindings were not found");
      const binding = await tx.binding(request);
      if (!binding)
        throw invalidCredential("join credential bindings were not found");
      validateBinding(binding, request, proof, requiredDatabaseNow(binding));
      const existing = await tx.issuance(request);
      if (existing)
        return {
          response: replay(
            existing,
            idempotencyKey,
            digest,
            key,
            requiredDatabaseNow(binding),
          ),
        };
      let intent = await tx.intent(request);
      if (intent) {
        verifyIntent(intent, idempotencyKey, digest);
      } else {
        const id = randomUUID();
        intent = {
          id,
          workspaceId: binding.workspaceId,
          enrollmentId: binding.enrollmentId,
          planId: binding.planId,
          nodeId: binding.nodeId,
          providerHandle: id,
          idempotencyKey,
          requestDigest: digest,
          status: "pending",
        };
        await tx.insertIntent(intent);
      }
      const owner = await tx.claimIntent(
        intent.id,
        this.providerTimeoutMs + 5_000,
      );
      return { binding, intent, owner };
    });
    if ("response" in prepared) return prepared.response!;
    const intent = prepared.intent!;
    if (!prepared.owner)
      return await this.waitForExisting(
        request,
        idempotencyKey,
        digest,
        key,
        proof,
      );
    try {
      await this.providerCall((signal) =>
        this.issuer.revoke(intent.providerHandle, signal),
      );
    } catch (error) {
      await this.setIntent(intent.id, "revoke_required").catch(() => {});
      throw error;
    }
    let external: IssuedWorkerCredential;
    try {
      const plan = prepared.binding!.canonicalPlan,
        cluster = record(plan.cluster, "plan.cluster"),
        now = requiredDatabaseNow(prepared.binding!),
        expiry = credentialExpiry(prepared.binding!),
        ttlSeconds = ttl(expiry, now);
      external = await this.providerCall((signal) =>
        this.issuer.issue(
          {
            issuanceId: intent.id,
            clusterId: text(cluster.id, "cluster.id", 128),
            expectedNodeName: text(plan.hostname, "hostname", 253),
            bootstrapTaint: "blazn.dev/bootstrap=pending:NoSchedule",
            ttlSeconds,
            workerOnly: true,
          },
          signal,
        ),
      );
      validateIssued(
        external,
        intent,
        expiry,
        cluster.id,
        now,
        ttlSeconds,
      );
    } catch (error) {
      await this.compensate(intent);
      throw error;
    }
    try {
      return await this.store.transaction(async (tx) => {
        await tx.lockNode(request.nodeId);
        if (!(await tx.lockBinding(request)))
          throw invalidCredential(
            "join credential bindings changed during issuance",
          );
        const binding = await tx.binding(request);
        if (!binding)
          throw invalidCredential(
            "join credential bindings changed during issuance",
          );
        const now = requiredDatabaseNow(binding);
        validateBinding(binding, request, proof, now);
        const existing = await tx.issuance(request);
        if (existing) return replay(existing, idempotencyKey, digest, key, now);
        const current = await tx.intent(request);
        if (!current) throw new Error("join issuance intent disappeared");
        verifyIntent(current, idempotencyKey, digest);
        const plan = binding.canonicalPlan,
          cluster = record(plan.cluster, "plan.cluster"),
          clusterId = text(cluster.id, "cluster.id", 128),
          expiry = credentialExpiry(binding),
          ttlSeconds = ttl(expiry, now);
        validateIssued(
          external,
          current,
          expiry,
          cluster.id,
          now,
          ttlSeconds,
        );
        const context = {
            workspaceId: binding.workspaceId,
            enrollmentId: binding.enrollmentId,
            planId: binding.planId,
            nodeId: binding.nodeId,
            issuanceId: current.id,
            idempotencyKey,
            requestDigest: digest,
          },
          ciphertext = sealJoinCredential(key, external.credential, context);
        await tx.insertIssuance({
          id: current.id,
          workspaceId: binding.workspaceId,
          enrollmentId: binding.enrollmentId,
          planId: binding.planId,
          nodeId: binding.nodeId,
          clusterId,
          nodePublicKeyFingerprint: request.nodePublicKeyFingerprint,
          machineFingerprint: request.machineFingerprint,
          credentialHash: credentialHash(external.credential),
          credentialCiphertext: ciphertext,
          credentialKeyId: "node-join-credential/v1",
          idempotencyKey,
          requestDigest: digest,
          issuedAt: now,
          expiresAt: external.expiresAt,
          consumedAt: null,
          revokedAt: null,
        });
        await tx.setIntentStatus(current.id, "completed");
        return {
          issuanceId: current.id,
          credential: external.credential,
          expiresAt: external.expiresAt.toISOString(),
          clusterId,
          workerOnly: true,
          replayed: false,
        };
      });
    } catch (error) {
      let recoveryError: unknown;
      const recovered = await this.recover(
        request,
        idempotencyKey,
        digest,
        key,
        proof,
      ).catch((cause: unknown) => {
        recoveryError = cause;
        return undefined;
      });
      if (recovered) return recovered;
      await this.compensate(intent);
      if (recoveryError instanceof NodeHttpError) throw recoveryError;
      throw mapStoreError(error);
    }
  }

  private async recover(
    request: JoinCredentialRequest,
    key: string,
    digest: string,
    encryptionKey: Buffer,
    proof: string,
  ): Promise<JoinCredentialResponse | undefined> {
    return this.store.transaction(async (tx) => {
      await tx.lockNode(request.nodeId);
      if (!(await tx.lockBinding(request))) return undefined;
      const binding = await tx.binding(request);
      if (!binding) return undefined;
      const now = requiredDatabaseNow(binding);
      validateBinding(binding, request, proof, now);
      const existing = await tx.issuance(request);
      return existing
        ? replay(
            existing,
            key,
            digest,
            encryptionKey,
            now,
          )
        : undefined;
    });
  }
  private async waitForExisting(
    request: JoinCredentialRequest,
    key: string,
    digest: string,
    encryptionKey: Buffer,
    proof: string,
  ): Promise<JoinCredentialResponse> {
    const deadline = Date.now() + this.providerTimeoutMs + 5_000;
    while (Date.now() < deadline) {
      const existing = await this.recover(
        request,
        key,
        digest,
        encryptionKey,
        proof,
      );
      if (existing) return existing;
      await new Promise((resolve) => setTimeout(resolve, 25));
    }
    throw new Error("join credential issuance is still in progress");
  }
  private async compensate(intent: BrokerIssuanceIntent) {
    await this.setIntent(intent.id, "revoke_required").catch(() => {});
    try {
      await this.providerCall((signal) =>
        this.issuer.revoke(intent.providerHandle, signal),
      );
      await this.setIntent(intent.id, "revoked");
    } catch {
      /* durable revoke_required intent is recovered on retry */
    }
  }
  private async setIntent(id: string, status: BrokerIssuanceIntent["status"]) {
    await this.store.transaction((tx) => tx.setIntentStatus(id, status));
  }
  private async providerCall<T>(
    action: (signal: AbortSignal) => Promise<T>,
  ): Promise<T> {
    const controller = new AbortController(),
      timeout = setTimeout(
        () =>
          controller.abort(
            new Error("worker credential provider deadline exceeded"),
          ),
        this.providerTimeoutMs,
      );
    try {
      return await Promise.race([
        action(controller.signal),
        new Promise<never>((_, reject) =>
          controller.signal.addEventListener(
            "abort",
            () => reject(controller.signal.reason),
            { once: true },
          ),
        ),
      ]);
    } finally {
      clearTimeout(timeout);
    }
  }
}

function validateRequest(key: string, r: JoinCredentialRequest, proof: string) {
  if (key.length < 8 || key.length > 128) invalid("Idempotency-Key is invalid");
  for (const [name, value] of [
    ["enrollmentId", r.enrollmentId],
    ["planId", r.planId],
    ["nodeId", r.nodeId],
  ] as const)
    if (!UUID.test(value)) invalid(`${name} is invalid`);
  if (
    !DIGEST.test(r.planDigest) ||
    !HEX.test(r.machineFingerprint) ||
    !DIGEST.test(r.nodePublicKeyFingerprint) ||
    !/^[A-Za-z0-9_-]{86}$/.test(proof)
  )
    invalid("join credential request binding is invalid");
}
function validateBinding(
  b: BrokerBinding,
  r: JoinCredentialRequest,
  proof: string,
  now: Date,
) {
  if (
    !b.enrollmentCreatedBy ||
    !b.planApprovedBy ||
    !b.nodeMachineFingerprint ||
    b.enrollmentId !== r.enrollmentId ||
    b.planId !== r.planId ||
    b.nodeId !== r.nodeId ||
    b.planDigest !== r.planDigest ||
    b.machineFingerprint !== r.machineFingerprint ||
    b.nodeMachineFingerprint !== r.machineFingerprint ||
    b.nodePublicKeyFingerprint !== r.nodePublicKeyFingerprint
  )
    throw invalidCredential(
      "join credential request does not match its enrollment and plan",
    );
  if (
    b.enrollmentStatus !== "exchanged" ||
    b.enrollmentExpiresAt.getTime() <= now.getTime() ||
    b.planStatus !== "issued" ||
    b.planExpiresAt.getTime() <= now.getTime() ||
    b.nodeLifecycleState !== "installing" ||
    b.nodeTrustState !== "verifying"
  )
    throw invalidCredential("join credential binding is not issuable");
  if (!verifyNodeProof(b.nodePublicKey, "blazn-node-join-v1", r, proof))
    throw new NodeHttpError(
      "identity_rejected",
      "node proof could not be verified",
    );
  const plan = b.canonicalPlan,
    profile = plan.installProfile;
  if (
    typeof profile !== "string" ||
    !NODE_INSTALL_PROFILES.includes(profile as NodeInstallProfile)
  )
    throw invalidCredential("install profile is invalid");
  try {
    validateSignedNodeInstallPlan(
      plan,
      profile as NodeInstallProfile,
      b.planSigningKeyId,
    );
  } catch {
    throw invalidCredential("stored install plan is invalid");
  }
  if (
    !verifyNodePlanSignature(
      b.planSigningPublicKey,
      String(plan.digest),
      String(plan.signature),
    )
  )
    throw invalidCredential("install plan signature is invalid");
  const cluster = record(plan.cluster, "plan.cluster"),
    target = record(plan.target, "plan.target");
  if (
    plan.workspaceId !== b.workspaceId ||
    plan.enrollmentId !== b.enrollmentId ||
    plan.planId !== b.planId ||
    plan.nodeId !== b.nodeId ||
    plan.approvedBy !== b.planApprovedBy ||
    b.planApprovedBy !== b.enrollmentCreatedBy ||
    plan.digest !== b.planDigest ||
    plan.expiresAt !== b.planExpiresAt.toISOString() ||
    cluster.workerOnly !== true ||
    cluster.joinCredentialEndpoint !== "/v1/node-service/join-credentials" ||
    cluster.bootstrapTaint !== "blazn.dev/bootstrap=pending:NoSchedule" ||
    target.machineFingerprint !== r.machineFingerprint ||
    target.nodePublicKeyFingerprint !== r.nodePublicKeyFingerprint
  )
    throw invalidCredential("stored install plan bindings are invalid");
}
function validateIssued(
  v: IssuedWorkerCredential,
  intent: BrokerIssuanceIntent,
  planExpiry: Date,
  clusterId: unknown,
  now: Date,
  ttlSeconds: number,
) {
  if (
    v.providerHandle !== intent.providerHandle ||
    v.workerOnly !== true ||
    v.clusterHealthy !== true ||
    v.clusterId !== clusterId ||
    v.credential.length < 43 ||
    v.credential.length > 4096 ||
    !(v.expiresAt instanceof Date) ||
    !Number.isFinite(v.expiresAt.getTime()) ||
    v.expiresAt.getTime() <= now.getTime() ||
    v.expiresAt.getTime() > now.getTime() + ttlSeconds * 1000 ||
    v.expiresAt.getTime() > planExpiry.getTime()
  )
    throw invalidCredential(
      "worker credential provider returned an invalid credential",
    );
}
function verifyIntent(v: BrokerIssuanceIntent, key: string, digest: string) {
  if (v.idempotencyKey !== key || v.requestDigest !== digest)
    throw new NodeHttpError(
      "idempotency_conflict",
      "join intent is bound to another request",
    );
  if (v.status === "completed")
    throw new Error("completed join intent has no issuance");
}
function replay(
  v: StoredJoinIssuance,
  key: string,
  digest: string,
  encryptionKey: Buffer,
  now: Date,
): JoinCredentialResponse {
  if (v.idempotencyKey !== key || v.requestDigest !== digest)
    throw new NodeHttpError(
      "idempotency_conflict",
      "join credential is bound to another request",
    );
  if (v.consumedAt)
    throw new NodeHttpError(
      "join_credential_consumed",
      "join credential was consumed",
    );
  if (v.revokedAt || v.expiresAt.getTime() <= now.getTime())
    throw invalidCredential("join credential is revoked or expired");
  let credential: string;
  try {
    credential = openJoinCredential(encryptionKey, v.credentialCiphertext, {
      workspaceId: v.workspaceId,
      enrollmentId: v.enrollmentId,
      planId: v.planId,
      nodeId: v.nodeId,
      issuanceId: v.id,
      idempotencyKey: v.idempotencyKey,
      requestDigest: v.requestDigest,
    });
  } catch {
    throw new Error("stored join credential could not be decrypted");
  }
  return {
    issuanceId: v.id,
    credential,
    expiresAt: v.expiresAt.toISOString(),
    clusterId: v.clusterId,
    workerOnly: true,
    replayed: true,
  };
}
function requiredDatabaseNow(b: BrokerBinding): Date {
  if (
    !(b.databaseNow instanceof Date) ||
    !Number.isFinite(b.databaseNow.getTime())
  )
    throw invalidCredential("database clock is unavailable");
  return b.databaseNow;
}
function ttl(expiry: Date, now: Date) {
  return Math.max(
    1,
    Math.min(300, Math.floor((expiry.getTime() - now.getTime()) / 1000)),
  );
}
function credentialExpiry(binding: BrokerBinding): Date {
  return binding.enrollmentExpiresAt.getTime() < binding.planExpiresAt.getTime()
    ? binding.enrollmentExpiresAt
    : binding.planExpiresAt;
}
const UUID =
    /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
  HEX = /^[0-9a-f]{64}$/,
  DIGEST = /^sha256:[0-9a-f]{64}$/;
function record(v: unknown, name: string): Record<string, unknown> {
  if (!v || typeof v !== "object" || Array.isArray(v))
    throw invalidCredential(`${name} is invalid`);
  return v as Record<string, unknown>;
}
function text(v: unknown, name: string, max: number): string {
  if (typeof v !== "string" || !v || v.length > max)
    throw invalidCredential(`${name} is invalid`);
  return v;
}
function invalid(message: string): never {
  throw new NodeHttpError("invalid_request", message);
}
function invalidCredential(message: string) {
  return new NodeHttpError("join_credential_invalid", message);
}
function mapStoreError(error: unknown): unknown {
  const e = error as { code?: string };
  if (e?.code === "23505")
    return new NodeHttpError(
      "idempotency_conflict",
      "join credential already exists for another request",
    );
  return error;
}

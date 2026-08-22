export interface JoinCredentialRequest {
  enrollmentId: string;
  planId: string;
  planDigest: string;
  nodeId: string;
  machineFingerprint: string;
  nodePublicKeyFingerprint: string;
}

export interface JoinCredentialResponse {
  issuanceId: string;
  credential: string;
  expiresAt: string;
  clusterId: string;
  workerOnly: true;
  replayed: boolean;
}

export interface WorkerCredentialIssueRequest {
  issuanceId: string;
  clusterId: string;
  expectedNodeName: string;
  bootstrapTaint: "blazn.dev/bootstrap=pending:NoSchedule";
  ttlSeconds: number;
  workerOnly: true;
}

export interface IssuedWorkerCredential {
  providerHandle: string;
  credential: string;
  clusterId: string;
  clusterHealthy: true;
  workerOnly: true;
  expiresAt: Date;
}

export interface WorkerCredentialIssuer {
  issue(
    request: WorkerCredentialIssueRequest,
    signal: AbortSignal,
  ): Promise<IssuedWorkerCredential>;
  revoke(providerHandle: string, signal: AbortSignal): Promise<void>;
}

export interface BrokerBinding {
  databaseNow?: Date;
  workspaceId: string;
  enrollmentId: string;
  enrollmentStatus: string;
  enrollmentExpiresAt: Date;
  enrollmentCreatedBy?: string;
  nodePublicKey: string;
  nodePublicKeyFingerprint: string;
  machineFingerprint: string;
  nodeId: string;
  nodeMachineFingerprint?: string;
  nodeLifecycleState: string;
  nodeTrustState: string;
  planId: string;
  planDigest: string;
  planStatus: string;
  planApprovedBy?: string;
  planExpiresAt: Date;
  canonicalPlan: Record<string, unknown>;
  planSigningKeyId: string;
  planSigningPublicKey: string;
}

export interface StoredJoinIssuance {
  id: string;
  workspaceId: string;
  enrollmentId: string;
  planId: string;
  nodeId: string;
  clusterId: string;
  machineFingerprint: string;
  nodePublicKeyFingerprint: string;
  credentialCiphertext: Buffer;
  credentialKeyId: "node-join-credential/v1";
  idempotencyKey: string;
  requestDigest: string;
  issuedAt: Date;
  expiresAt: Date;
  consumedAt: Date | null;
  revokedAt: Date | null;
}

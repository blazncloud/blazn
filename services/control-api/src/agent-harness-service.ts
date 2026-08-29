import { randomUUID } from "node:crypto";
import { requestDigest } from "./workspace-crypto.js";
import type { IdempotencyReceipt } from "./workspace-store.js";
import { roleAllows } from "./workspace-types.js";
import { agentVersionDigest, harnessSecretViolations, verifyAgentCompatibility, verifyHarnessBundle, verifyHarnessVersion } from "./harness-contract.js";
import { validateAgentBundleSchema, validateHarnessDefinitionSchema, validateHarnessProfileSchema, validateHarnessVersionSchema } from "./agent-harness-validation.js";
import { AgentNameConflictError, AgentVersionConflictError, DefinitionKindConflictError, HarnessVersionConflictError, IdentityConflictError, ProfileNameConflictError, ProfileRevisionConflictError, type AgentHarnessStore, type AgentHarnessTransaction } from "./agent-harness-store.js";
import { AgentHarnessHttpError, type Agent, type AgentHarnessPrincipal, type CreateAgentInput, type CreateHarnessDefinitionInput, type CreateHarnessProfileInput, type HarnessProfile, type JsonDocument, type PublishAgentVersionInput, type PublishHarnessVersionInput, type ReviseHarnessProfileInput } from "./agent-harness-types.js";

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const NAME = /^[a-z][a-z0-9._-]{0,95}$/;
const AGENT_SCHEMA_VERSION = "blazn.dev/agent/v1alpha1";

export class AgentHarnessService {
  constructor(private readonly store: AgentHarnessStore) {}

  async createAgent(principal: AgentHarnessPrincipal, workspaceId: string, key: string, input: CreateAgentInput) {
    const normalized = validateCreateAgent(input);
    const digest = requestDigest({ workspaceId, ...normalized });
    return this.idempotent(principal, workspaceId, "agent.create", key, `workspace:${workspaceId}:agents:${normalized.name}`, digest, async (tx) => {
      const agent = await mapped(() => tx.createAgent({ id: randomUUID(), workspaceId, ownerId: principal.userId, ...normalized, createdBy: principal.userId }));
      await tx.insertAudit(randomUUID(), workspaceId, principal.userId, "agent.created", { agentId: agent.id, name: agent.name });
      return { agent };
    }, 201);
  }
  async listAgents(principal: AgentHarnessPrincipal, workspaceId: string, cursor = "") {
    return this.store.transaction(async (tx) => { await this.authorize(tx, principal, workspaceId, "read"); return tx.listAgents(workspaceId, cursor); });
  }
  async getAgent(principal: AgentHarnessPrincipal, workspaceId: string, agentId: string) {
    return this.store.transaction(async (tx) => {
      await this.authorize(tx, principal, workspaceId, "read");
      const agent = await tx.getAgent(workspaceId, agentId);
      if (!agent) throw new AgentHarnessHttpError("agent_not_found", "Agent was not found");
      return { agent };
    });
  }

  async publishAgentVersion(principal: AgentHarnessPrincipal, workspaceId: string, agentId: string, key: string, input: PublishAgentVersionInput) {
    const document = documentInput(input.version, "version");
    const digest = requestDigest({ workspaceId, agentId, document });
    return this.idempotent(principal, workspaceId, "agent.version.publish", key, `agent:${agentId}:versions`, digest, async (tx) => {
      const agent = await tx.getAgent(workspaceId, agentId, true);
      if (!agent) throw new AgentHarnessHttpError("agent_not_found", "Agent was not found");
      if (agent.status !== "active") throw new AgentHarnessHttpError("invalid_request", "AgentVersion publication requires an active Agent");
      const id = text(document.id), declaredVersion = document.version;
      if (!UUID.test(id)) invalid("AgentVersion id must be a UUID");
      if (document.agentId !== agentId || document.workspaceId !== workspaceId) throw new AgentHarnessHttpError("identity_conflict", "AgentVersion identity does not match the addressed Agent");
      if (document.createdBy !== principal.userId) invalid("AgentVersion createdBy must be the publishing principal");
      const createdAtMs = Date.parse(text(document.createdAt));
      if (!Number.isFinite(createdAtMs) || createdAtMs > Date.now() + 300_000) invalid("AgentVersion createdAt must be a valid timestamp not in the future");
      const nextVersion = await tx.maxAgentVersionNumber(agentId) + 1;
      if (declaredVersion !== nextVersion) throw new AgentHarnessHttpError("agent_version_sequence_conflict", `AgentVersion version must be the next sequence value ${nextVersion}`);
      if (document.digest !== agentVersionDigest(document)) throw new AgentHarnessHttpError("contract_violation", "AgentVersion semantic digest is invalid");
      const composedAgent: JsonDocument = { schemaVersion: AGENT_SCHEMA_VERSION, id: agent.id, workspaceId: agent.workspaceId, ownerId: agent.ownerId, name: agent.name, tags: agent.tags, status: agent.status, currentVersionId: id, version: agent.version + 1 };
      const composed = { agent: composedAgent, version: document };
      const schemaErrors = validateAgentBundleSchema(composed);
      if (schemaErrors.length) throw new AgentHarnessHttpError("contract_violation", "AgentVersion document does not satisfy the Agent schema", schemaErrors);
      const bundles = await this.resolveAllowedProfiles(tx, workspaceId, document);
      const contractErrors = verifyAgentCompatibility(composed, bundles);
      if (contractErrors.length) throw new AgentHarnessHttpError("contract_violation", "AgentVersion is not compatible with its allowed HarnessProfiles", contractErrors);
      await this.verifyTemplateBinding(tx, workspaceId, document);
      const version = await mapped(() => tx.insertAgentVersion({ id, agentId, workspaceId, version: nextVersion, digest: text(document.digest), document, createdBy: principal.userId }));
      const updated = await tx.setAgentCurrentVersion(agent, id);
      if (!updated) throw new AgentHarnessHttpError("version_conflict", "Agent version changed");
      await tx.insertAudit(randomUUID(), workspaceId, principal.userId, "agent.version_published", { agentId, agentVersionId: id, version: nextVersion, digest: version.digest });
      return { agent: updated, version };
    }, 201);
  }
  async listAgentVersions(principal: AgentHarnessPrincipal, workspaceId: string, agentId: string, cursor = "") {
    return this.store.transaction(async (tx) => {
      await this.authorize(tx, principal, workspaceId, "read");
      if (!await tx.getAgent(workspaceId, agentId)) throw new AgentHarnessHttpError("agent_not_found", "Agent was not found");
      return tx.listAgentVersions(workspaceId, agentId, cursor);
    });
  }
  async getAgentVersion(principal: AgentHarnessPrincipal, workspaceId: string, agentId: string, versionId: string) {
    return this.store.transaction(async (tx) => {
      await this.authorize(tx, principal, workspaceId, "read");
      const version = await tx.getAgentVersion(workspaceId, agentId, versionId);
      if (!version) throw new AgentHarnessHttpError("agent_version_not_found", "AgentVersion was not found");
      return { version };
    });
  }

  async createHarnessDefinition(principal: AgentHarnessPrincipal, workspaceId: string, key: string, input: CreateHarnessDefinitionInput) {
    const document = documentInput(input.definition, "definition");
    const digest = requestDigest({ workspaceId, document });
    return this.idempotent(principal, workspaceId, "harness.definition.create", key, `workspace:${workspaceId}:harness-definitions:${text(document.kind)}`, digest, async (tx) => {
      const id = text(document.id);
      if (!UUID.test(id)) invalid("HarnessDefinition id must be a UUID");
      if (document.resourceVersion !== 1) invalid("HarnessDefinition resourceVersion must start at 1");
      const schemaErrors = validateHarnessDefinitionSchema(document);
      if (schemaErrors.length) throw new AgentHarnessHttpError("contract_violation", "HarnessDefinition document does not satisfy the Harness schema", schemaErrors);
      const secretErrors = harnessSecretViolations(document);
      if (secretErrors.length) throw new AgentHarnessHttpError("contract_violation", "HarnessDefinition contains credential-like material", secretErrors);
      const definition = await mapped(() => tx.createHarnessDefinition({ id, workspaceId, kind: text(document.kind), status: text(document.status), resourceVersion: 1, document, createdBy: principal.userId }));
      await tx.insertAudit(randomUUID(), workspaceId, principal.userId, "harness.definition_created", { definitionId: id, kind: definition.kind });
      return { definition };
    }, 201);
  }
  async listHarnessDefinitions(principal: AgentHarnessPrincipal, workspaceId: string, cursor = "") {
    return this.store.transaction(async (tx) => { await this.authorize(tx, principal, workspaceId, "read"); return tx.listHarnessDefinitions(workspaceId, cursor); });
  }
  async getHarnessDefinition(principal: AgentHarnessPrincipal, workspaceId: string, definitionId: string) {
    return this.store.transaction(async (tx) => {
      await this.authorize(tx, principal, workspaceId, "read");
      const definition = await tx.getHarnessDefinition(workspaceId, definitionId);
      if (!definition) throw new AgentHarnessHttpError("definition_not_found", "HarnessDefinition was not found");
      return { definition };
    });
  }

  async publishHarnessVersion(principal: AgentHarnessPrincipal, workspaceId: string, definitionId: string, key: string, input: PublishHarnessVersionInput) {
    const document = documentInput(input.version, "version");
    const digest = requestDigest({ workspaceId, definitionId, document });
    return this.idempotent(principal, workspaceId, "harness.version.publish", key, `harness-definition:${definitionId}:versions`, digest, async (tx) => {
      const definition = await tx.getHarnessDefinition(workspaceId, definitionId, true);
      if (!definition) throw new AgentHarnessHttpError("definition_not_found", "HarnessDefinition was not found");
      if (definition.status !== "approved") invalid("HarnessVersion publication requires an approved HarnessDefinition");
      const id = text(document.id);
      if (!UUID.test(id)) invalid("HarnessVersion id must be a UUID");
      if (document.definitionId !== definitionId) throw new AgentHarnessHttpError("identity_conflict", "HarnessVersion identity does not match the addressed HarnessDefinition");
      const schemaErrors = validateHarnessVersionSchema(document);
      if (schemaErrors.length) throw new AgentHarnessHttpError("contract_violation", "HarnessVersion document does not satisfy the Harness schema", schemaErrors);
      const secretErrors = harnessSecretViolations(document);
      if (secretErrors.length) throw new AgentHarnessHttpError("contract_violation", "HarnessVersion contains credential-like material", secretErrors);
      const definitionPlatforms = new Set(stringList(definition.document.supportedPlatforms));
      const platformErrors = stringList(document.supportedPlatforms).filter((platform) => !definitionPlatforms.has(platform));
      const contractErrors = [...verifyHarnessVersion(definition.document, document), ...platformErrors.map((platform) => `HarnessVersion platform ${platform} exceeds HarnessDefinition`)];
      if (contractErrors.length) throw new AgentHarnessHttpError("contract_violation", "HarnessVersion violates the Harness contract", contractErrors);
      const version = await mapped(() => tx.insertHarnessVersion({ id, definitionId, workspaceId, version: text(document.version), digest: text(document.digest), document, createdBy: principal.userId }));
      await tx.insertAudit(randomUUID(), workspaceId, principal.userId, "harness.version_published", { definitionId, harnessVersionId: id, version: version.version, digest: version.digest });
      return { version };
    }, 201);
  }
  async listHarnessVersions(principal: AgentHarnessPrincipal, workspaceId: string, definitionId: string, cursor = "") {
    return this.store.transaction(async (tx) => {
      await this.authorize(tx, principal, workspaceId, "read");
      if (!await tx.getHarnessDefinition(workspaceId, definitionId)) throw new AgentHarnessHttpError("definition_not_found", "HarnessDefinition was not found");
      return tx.listHarnessVersions(workspaceId, definitionId, cursor);
    });
  }
  async getHarnessVersion(principal: AgentHarnessPrincipal, workspaceId: string, definitionId: string, versionId: string) {
    return this.store.transaction(async (tx) => {
      await this.authorize(tx, principal, workspaceId, "read");
      const version = await tx.getHarnessVersion(workspaceId, versionId);
      if (!version || version.definitionId !== definitionId) throw new AgentHarnessHttpError("harness_version_not_found", "HarnessVersion was not found");
      return { version };
    });
  }

  async createHarnessProfile(principal: AgentHarnessPrincipal, workspaceId: string, key: string, input: CreateHarnessProfileInput) {
    const document = documentInput(input.profile, "profile");
    const digest = requestDigest({ workspaceId, document });
    return this.idempotent(principal, workspaceId, "harness.profile.create", key, `workspace:${workspaceId}:harness-profiles:${text(document.name)}`, digest, async (tx) => {
      const id = text(document.id);
      if (!UUID.test(id)) invalid("HarnessProfile id must be a UUID");
      if (document.workspaceId !== workspaceId) throw new AgentHarnessHttpError("identity_conflict", "HarnessProfile identity does not match the addressed Workspace");
      if (document.resourceVersion !== 1) invalid("HarnessProfile resourceVersion must start at 1");
      await this.validateProfileDocument(tx, workspaceId, document);
      const profile = await mapped(() => tx.createHarnessProfile({ id, workspaceId, name: text(document.name), harnessVersionId: text(document.harnessVersionId), status: text(document.status), resourceVersion: 1, digest: text(document.digest), document, createdBy: principal.userId }));
      await mapped(() => tx.insertHarnessProfileRevision({ id: randomUUID(), profileId: id, workspaceId, resourceVersion: 1, digest: profile.digest, document, createdBy: principal.userId }));
      await tx.insertAudit(randomUUID(), workspaceId, principal.userId, "harness.profile_created", { profileId: id, name: profile.name, digest: profile.digest });
      return { profile };
    }, 201);
  }
  async reviseHarnessProfile(principal: AgentHarnessPrincipal, workspaceId: string, profileId: string, key: string, input: ReviseHarnessProfileInput) {
    const document = documentInput(input.profile, "profile");
    if (!Number.isSafeInteger(input.expectedResourceVersion) || input.expectedResourceVersion < 1) invalid("expectedResourceVersion must be a positive integer");
    const digest = requestDigest({ workspaceId, profileId, expectedResourceVersion: input.expectedResourceVersion, document });
    return this.idempotent(principal, workspaceId, "harness.profile.revise", key, `harness-profile:${profileId}`, digest, async (tx) => {
      const current = await tx.getHarnessProfile(workspaceId, profileId, true);
      if (!current) throw new AgentHarnessHttpError("profile_not_found", "HarnessProfile was not found");
      if (current.resourceVersion !== input.expectedResourceVersion) throw new AgentHarnessHttpError("profile_revision_conflict", "HarnessProfile resourceVersion changed");
      if (document.id !== profileId || document.workspaceId !== workspaceId) throw new AgentHarnessHttpError("identity_conflict", "HarnessProfile identity does not match the addressed profile");
      if (document.resourceVersion !== current.resourceVersion + 1) throw new AgentHarnessHttpError("profile_revision_conflict", `HarnessProfile resourceVersion must be ${current.resourceVersion + 1}`);
      await this.validateProfileDocument(tx, workspaceId, document);
      const profile = await mapped(() => tx.reviseHarnessProfile(current, { name: text(document.name), harnessVersionId: text(document.harnessVersionId), status: text(document.status), resourceVersion: current.resourceVersion + 1, digest: text(document.digest), document, revisedBy: principal.userId }));
      if (!profile) throw new AgentHarnessHttpError("profile_revision_conflict", "HarnessProfile resourceVersion changed");
      await mapped(() => tx.insertHarnessProfileRevision({ id: randomUUID(), profileId, workspaceId, resourceVersion: profile.resourceVersion, digest: profile.digest, document, createdBy: principal.userId }));
      await tx.insertAudit(randomUUID(), workspaceId, principal.userId, "harness.profile_revised", { profileId, resourceVersion: profile.resourceVersion, digest: profile.digest });
      return { profile };
    }, 200);
  }
  async listHarnessProfiles(principal: AgentHarnessPrincipal, workspaceId: string, cursor = "") {
    return this.store.transaction(async (tx) => { await this.authorize(tx, principal, workspaceId, "read"); return tx.listHarnessProfiles(workspaceId, cursor); });
  }
  async getHarnessProfile(principal: AgentHarnessPrincipal, workspaceId: string, profileId: string) {
    return this.store.transaction(async (tx) => {
      await this.authorize(tx, principal, workspaceId, "read");
      const profile = await tx.getHarnessProfile(workspaceId, profileId);
      if (!profile) throw new AgentHarnessHttpError("profile_not_found", "HarnessProfile was not found");
      return { profile };
    });
  }

  private async validateProfileDocument(tx: AgentHarnessTransaction, workspaceId: string, document: JsonDocument): Promise<void> {
    const schemaErrors = validateHarnessProfileSchema(document);
    if (schemaErrors.length) throw new AgentHarnessHttpError("contract_violation", "HarnessProfile document does not satisfy the Harness schema", schemaErrors);
    const version = await tx.getHarnessVersion(workspaceId, text(document.harnessVersionId));
    if (!version) throw new AgentHarnessHttpError("harness_version_not_found", "HarnessProfile selects an unknown HarnessVersion");
    const definition = await tx.getHarnessDefinition(workspaceId, version.definitionId);
    if (!definition) throw new AgentHarnessHttpError("definition_not_found", "HarnessVersion definition was not found");
    let bundleErrors = verifyHarnessBundle({ definition: definition.document, version: version.document, profile: document });
    // A disabled profile is deliberately not executable; the bundle verifier's approval
    // check guards Run selection, not persistence, so disabling stays writable as long as
    // the definition itself is still approved.
    if (document.status === "disabled" && definition.status === "approved") bundleErrors = bundleErrors.filter((error) => error !== "Harness definition and profile must be approved");
    if (bundleErrors.length) throw new AgentHarnessHttpError("contract_violation", "HarnessProfile violates the Harness contract", bundleErrors);
  }
  private async resolveAllowedProfiles(tx: AgentHarnessTransaction, workspaceId: string, document: JsonDocument): Promise<JsonDocument[]> {
    const refs = Array.isArray(document.allowedHarnessProfiles) ? document.allowedHarnessProfiles : [];
    const bundles: JsonDocument[] = [];
    for (const raw of refs) {
      const ref = raw && typeof raw === "object" && !Array.isArray(raw) ? raw as JsonDocument : {};
      const profileId = text(ref.id);
      if (!UUID.test(profileId)) throw new AgentHarnessHttpError("contract_violation", "AgentVersion allowed HarnessProfile pin is not a UUID");
      const profile = await tx.getHarnessProfile(workspaceId, profileId);
      if (!profile) throw new AgentHarnessHttpError("contract_violation", `AgentVersion allowed HarnessProfile ${profileId} was not found in this Workspace`);
      const version = await tx.getHarnessVersion(workspaceId, profile.harnessVersionId);
      if (!version) throw new AgentHarnessHttpError("contract_violation", `HarnessProfile ${profileId} selects an unresolved HarnessVersion`);
      const definition = await tx.getHarnessDefinition(workspaceId, version.definitionId);
      if (!definition) throw new AgentHarnessHttpError("contract_violation", `HarnessProfile ${profileId} resolves an unresolved HarnessDefinition`);
      bundles.push({ definition: definition.document, version: version.document, profile: profile.document });
    }
    return bundles;
  }
  private async verifyTemplateBinding(tx: AgentHarnessTransaction, workspaceId: string, document: JsonDocument): Promise<void> {
    const template = document.template && typeof document.template === "object" && !Array.isArray(document.template) ? document.template as JsonDocument : {};
    const versionId = text(template.versionId);
    const stored = await tx.getTemplateVersionDigest(workspaceId, versionId);
    if (!stored) throw new AgentHarnessHttpError("template_version_not_found", "AgentVersion template version was not found in this Workspace");
    if (`sha256:${stored}` !== template.digest) throw new AgentHarnessHttpError("contract_violation", "AgentVersion template digest does not match the published SandboxTemplateVersion");
  }

  private async authorize(tx: AgentHarnessTransaction, principal: AgentHarnessPrincipal, workspaceId: string, capability: "read" | "operate", lock = false): Promise<void> {
    const access = await tx.getAccess(workspaceId, principal.userId, lock);
    if (!access || access.workspaceStatus !== "active") throw new AgentHarnessHttpError("workspace_not_found", "Workspace was not found");
    if (!roleAllows(access.role, capability)) throw new AgentHarnessHttpError("permission_denied", "Agent or Harness action is not permitted");
  }
  private async idempotent<T>(principal: AgentHarnessPrincipal, workspaceId: string, operation: string, key: string, targetKey: string, digest: string, execute: (tx: AgentHarnessTransaction) => Promise<T>, status: number): Promise<T> {
    validKey(key);
    return this.store.transaction(async (tx) => {
      await tx.lockIdempotency(principal.userId, operation, key);
      const receipt = await tx.getIdempotency(principal.userId, operation, key);
      if (receipt) {
        verifyReceipt(receipt, workspaceId, targetKey, digest);
        await this.authorize(tx, principal, workspaceId, "operate", true);
        return receipt.responseBody as T;
      }
      await this.authorize(tx, principal, workspaceId, "operate", true);
      const response = await execute(tx);
      await tx.putIdempotency(principal.userId, operation, key, { workspaceId, targetKey, requestDigest: digest, responseStatus: status, responseBody: response });
      return response;
    });
  }
}

function validateCreateAgent(input: CreateAgentInput): CreateAgentInput {
  if (!NAME.test(input.name)) invalid("Agent name is invalid");
  if (!Array.isArray(input.tags) || input.tags.length > 32 || input.tags.some((tag) => !NAME.test(tag)) || new Set(input.tags).size !== input.tags.length) invalid("Agent tags are invalid");
  if (harnessSecretViolations({ name: input.name, tags: input.tags }).length) invalid("Agent name and tags must not resemble credential material");
  return { name: input.name, tags: [...input.tags] };
}
function documentInput(value: unknown, field: string): JsonDocument {
  if (!value || typeof value !== "object" || Array.isArray(value)) invalid(`${field} document is invalid`);
  const serialized = JSON.stringify(value);
  if (Buffer.byteLength(serialized, "utf8") > 262144) invalid(`${field} document exceeds 262144 bytes`);
  return value as JsonDocument;
}
async function mapped<T>(action: () => Promise<T>): Promise<T> {
  try { return await action(); }
  catch (error) {
    if (error instanceof AgentNameConflictError) throw new AgentHarnessHttpError("agent_name_conflict", "Agent name is already used in this Workspace");
    if (error instanceof AgentVersionConflictError) throw new AgentHarnessHttpError("agent_version_sequence_conflict", "AgentVersion sequence or digest already exists");
    if (error instanceof DefinitionKindConflictError) throw new AgentHarnessHttpError("definition_kind_conflict", "HarnessDefinition kind already exists in this Workspace");
    if (error instanceof HarnessVersionConflictError) throw new AgentHarnessHttpError("harness_version_conflict", "HarnessVersion version or digest already exists");
    if (error instanceof ProfileNameConflictError) throw new AgentHarnessHttpError("profile_name_conflict", "HarnessProfile name is already used in this Workspace");
    if (error instanceof ProfileRevisionConflictError) throw new AgentHarnessHttpError("profile_revision_conflict", "HarnessProfile revision already exists");
    if (error instanceof IdentityConflictError) throw new AgentHarnessHttpError("identity_conflict", "resource ID already exists");
    throw error;
  }
}
function stringList(value: unknown): string[] { return Array.isArray(value) && value.every((item) => typeof item === "string") ? value : []; }
function text(value: unknown): string { return typeof value === "string" ? value : ""; }
function validKey(value: string): void { if (typeof value !== "string" || value.length < 8 || value.length > 128) invalid("Idempotency-Key is invalid"); }
function verifyReceipt(receipt: IdempotencyReceipt, workspaceId: string, targetKey: string, digest: string): void {
  if (receipt.workspaceId !== workspaceId || receipt.targetKey !== targetKey || receipt.requestDigest !== digest) throw new AgentHarnessHttpError("idempotency_conflict", "Idempotency key is bound to another request");
}
function invalid(message: string): never { throw new AgentHarnessHttpError("invalid_request", message); }

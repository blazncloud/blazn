import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { AgentHarnessService } from "./agent-harness-service.js";
import { agentSchemaSourceSha256, harnessSchemaSourceSha256 } from "./agent-harness-schemas.generated.js";
import { AgentNameConflictError, ProfileNameConflictError, type AgentHarnessStore, type AgentHarnessTransaction } from "./agent-harness-store.js";
import { AgentHarnessHttpError, type Agent, type AgentHarnessPrincipal, type AgentVersion, type HarnessDefinition, type HarnessProfile, type HarnessVersion, type JsonDocument } from "./agent-harness-types.js";
import type { IdempotencyReceipt } from "./workspace-store.js";

const here = path.dirname(fileURLToPath(import.meta.url));
const contracts = path.resolve(here, "../../../packages/contracts");
const fixtures = path.join(contracts, "testdata/harness");
const json = async (file: string) => JSON.parse(await readFile(file, "utf8")) as Record<string, unknown>;

const workspaceId = "40000000-0000-4000-8000-000000000001";
const publisher: AgentHarnessPrincipal = { userId: "10000000-0000-4000-8000-000000000099", email: "publisher@example.test", displayName: "publisher" };
const viewer: AgentHarnessPrincipal = { userId: "10000000-0000-4000-8000-000000000098", email: "viewer@example.test", displayName: "viewer" };

class FakeState {
  receipts = new Map<string, IdempotencyReceipt>();
  roles = new Map<string, string>([[publisher.userId, "owner"], [viewer.userId, "viewer"]]);
  agents = new Map<string, Agent>();
  agentVersions = new Map<string, AgentVersion>();
  definitions = new Map<string, HarnessDefinition>();
  versions = new Map<string, HarnessVersion>();
  profiles = new Map<string, HarnessProfile>();
  revisions: { profileId: string; resourceVersion: number }[] = [];
  templates = new Map<string, string>();
  audits: string[] = [];
}

class FakeTransaction implements AgentHarnessTransaction {
  constructor(private readonly state: FakeState) {}
  async lockIdempotency() {}
  async getIdempotency(principalId: string, operation: string, key: string) { return this.state.receipts.get(`${principalId}|${operation}|${key}`); }
  async putIdempotency(principalId: string, operation: string, key: string, receipt: IdempotencyReceipt) { this.state.receipts.set(`${principalId}|${operation}|${key}`, receipt); }
  async getAccess(target: string, userId: string) {
    if (target !== workspaceId) return undefined;
    const role = this.state.roles.get(userId);
    return role ? { workspaceStatus: "active" as const, role: role as "owner" } : undefined;
  }
  async createAgent(input: { id: string; workspaceId: string; ownerId: string; name: string; tags: string[]; createdBy: string }) {
    if ([...this.state.agents.values()].some((agent) => agent.name === input.name)) throw new AgentNameConflictError();
    const agent: Agent = { id: input.id, workspaceId: input.workspaceId, ownerId: input.ownerId, name: input.name, tags: input.tags, status: "active", currentVersionId: null, version: 1, createdAt: new Date().toISOString(), updatedAt: new Date().toISOString() };
    this.state.agents.set(input.id, agent);
    return agent;
  }
  async getAgent(_: string, agentId: string) { return this.state.agents.get(agentId); }
  async listAgents() { return [...this.state.agents.values()]; }
  async insertAgentVersion(input: { id: string; agentId: string; workspaceId: string; version: number; digest: string; document: JsonDocument; createdBy: string }) {
    const version: AgentVersion = { ...input, createdAt: new Date().toISOString() };
    this.state.agentVersions.set(input.id, version);
    return version;
  }
  async setAgentCurrentVersion(agent: Agent, versionId: string) {
    const current = this.state.agents.get(agent.id);
    if (!current || current.version !== agent.version) return undefined;
    const updated: Agent = { ...current, currentVersionId: versionId, version: current.version + 1 };
    this.state.agents.set(agent.id, updated);
    return updated;
  }
  async getAgentVersion(_: string, agentId: string, versionId: string) { const version = this.state.agentVersions.get(versionId); return version && version.agentId === agentId ? version : undefined; }
  async listAgentVersions(_: string, agentId: string) { return [...this.state.agentVersions.values()].filter((version) => version.agentId === agentId); }
  async maxAgentVersionNumber(agentId: string) { return Math.max(0, ...[...this.state.agentVersions.values()].filter((version) => version.agentId === agentId).map((version) => version.version)); }
  async createHarnessDefinition(input: { id: string; workspaceId: string; kind: string; status: string; resourceVersion: number; document: JsonDocument; createdBy: string }) {
    const definition: HarnessDefinition = { id: input.id, workspaceId: input.workspaceId, kind: input.kind as HarnessDefinition["kind"], status: input.status as HarnessDefinition["status"], resourceVersion: input.resourceVersion, document: input.document, createdAt: new Date().toISOString(), updatedAt: new Date().toISOString() };
    this.state.definitions.set(input.id, definition);
    return definition;
  }
  async getHarnessDefinition(_: string, definitionId: string) { return this.state.definitions.get(definitionId); }
  async listHarnessDefinitions() { return [...this.state.definitions.values()]; }
  async insertHarnessVersion(input: { id: string; definitionId: string; workspaceId: string; version: string; digest: string; document: JsonDocument; createdBy: string }) {
    const version: HarnessVersion = { ...input, createdAt: new Date().toISOString() };
    this.state.versions.set(input.id, version);
    return version;
  }
  async getHarnessVersion(_: string, versionId: string) { return this.state.versions.get(versionId); }
  async listHarnessVersions(_: string, definitionId: string) { return [...this.state.versions.values()].filter((version) => version.definitionId === definitionId); }
  async createHarnessProfile(input: { id: string; workspaceId: string; name: string; harnessVersionId: string; status: string; resourceVersion: number; digest: string; document: JsonDocument; createdBy: string }) {
    if ([...this.state.profiles.values()].some((profile) => profile.name === input.name)) throw new ProfileNameConflictError();
    const profile: HarnessProfile = { id: input.id, workspaceId: input.workspaceId, name: input.name, harnessVersionId: input.harnessVersionId, status: input.status as HarnessProfile["status"], resourceVersion: input.resourceVersion, digest: input.digest, document: input.document, createdAt: new Date().toISOString(), updatedAt: new Date().toISOString() };
    this.state.profiles.set(input.id, profile);
    return profile;
  }
  async reviseHarnessProfile(profile: HarnessProfile, input: { name: string; harnessVersionId: string; status: string; resourceVersion: number; digest: string; document: JsonDocument; revisedBy: string }) {
    const current = this.state.profiles.get(profile.id);
    if (!current || current.resourceVersion !== profile.resourceVersion) return undefined;
    const updated: HarnessProfile = { ...current, name: input.name, harnessVersionId: input.harnessVersionId, status: input.status as HarnessProfile["status"], resourceVersion: input.resourceVersion, digest: input.digest, document: input.document };
    this.state.profiles.set(profile.id, updated);
    return updated;
  }
  async insertHarnessProfileRevision(input: { id: string; profileId: string; workspaceId: string; resourceVersion: number; digest: string; document: JsonDocument; createdBy: string }) {
    this.state.revisions.push({ profileId: input.profileId, resourceVersion: input.resourceVersion });
  }
  async getHarnessProfile(_: string, profileId: string) { return this.state.profiles.get(profileId); }
  async listHarnessProfiles() { return [...this.state.profiles.values()]; }
  async getTemplateVersionDigest(_: string, templateVersionId: string) { return this.state.templates.get(templateVersionId); }
  async insertAudit(_: string, __: string, ___: string, type: string) { this.state.audits.push(type); }
}

class FakeStore implements AgentHarnessStore {
  constructor(readonly state = new FakeState()) {}
  async transaction<T>(action: (transaction: AgentHarnessTransaction) => Promise<T>): Promise<T> { return action(new FakeTransaction(this.state)); }
}

const isCode = (code: string) => (error: unknown) => error instanceof AgentHarnessHttpError && error.code === code;

test("embedded Agent and Harness schemas match the normative contract files", async () => {
  const agentSource = await readFile(path.join(contracts, "agent.schema.json"), "utf8");
  const harnessSource = await readFile(path.join(contracts, "harness.schema.json"), "utf8");
  assert.equal(createHash("sha256").update(agentSource).digest("hex"), agentSchemaSourceSha256, "agent.schema.json changed; regenerate with node scripts/generate-agent-harness-schemas.mjs");
  assert.equal(createHash("sha256").update(harnessSource).digest("hex"), harnessSchemaSourceSha256, "harness.schema.json changed; regenerate with node scripts/generate-agent-harness-schemas.mjs");
});

test("Agent and Harness publication persists the fixture chain with digests, tenancy, and idempotency", async () => {
  const store = new FakeStore();
  const service = new AgentHarnessService(store);
  const bundles = await Promise.all(["hermes-profile.json", "codex-profile.json", "claude-profile.json", "generic-profile.json"].map((name) => json(path.join(fixtures, name))));
  const agentFixture = await json(path.join(fixtures, "agent-good.json"));
  const agentDocument = agentFixture.agent as JsonDocument;
  const versionDocument = agentFixture.version as JsonDocument;
  store.state.templates.set((versionDocument.template as JsonDocument).versionId as string, ((versionDocument.template as JsonDocument).digest as string).slice("sha256:".length));

  const hermesDefinition = bundles[0]!.definition as JsonDocument;
  const created = await service.createHarnessDefinition(publisher, workspaceId, "definition-key-1", { definition: hermesDefinition });
  assert.equal(created.definition.kind, "hermes");
  const replay = await service.createHarnessDefinition(publisher, workspaceId, "definition-key-1", { definition: hermesDefinition });
  assert.equal(replay.definition.id, created.definition.id);
  await assert.rejects(() => service.createHarnessDefinition(publisher, workspaceId, "definition-key-1", { definition: bundles[1]!.definition as JsonDocument }), isCode("idempotency_conflict"));
  await assert.rejects(() => service.createHarnessDefinition(viewer, workspaceId, "definition-key-viewer", { definition: bundles[1]!.definition as JsonDocument }), isCode("permission_denied"));

  for (const [index, bundle] of bundles.entries()) {
    if (index > 0) await service.createHarnessDefinition(publisher, workspaceId, `definition-key-${index + 1}`, { definition: bundle.definition as JsonDocument });
    await service.publishHarnessVersion(publisher, workspaceId, (bundle.definition as JsonDocument).id as string, `version-key-${index + 1}`, { version: bundle.version as JsonDocument });
    await service.createHarnessProfile(publisher, workspaceId, `profile-key-${index + 1}`, { profile: bundle.profile as JsonDocument });
  }
  assert.equal((await service.listHarnessProfiles(viewer, workspaceId)).items.length, 4);

  const tamperedVersion = structuredClone(bundles[0]!.version) as JsonDocument;
  tamperedVersion.parserVersion = "hermes/v2";
  await assert.rejects(() => service.publishHarnessVersion(publisher, workspaceId, hermesDefinition.id as string, "version-key-tampered", { version: tamperedVersion }), isCode("contract_violation"));
  const badSecret = await json(path.join(fixtures, "profile-bad-secret.json"));
  await assert.rejects(() => service.createHarnessProfile(publisher, workspaceId, "profile-key-secret", { profile: badSecret.profile as JsonDocument }), isCode("contract_violation"));

  const seededAgent: Agent = { id: agentDocument.id as string, workspaceId, ownerId: agentDocument.ownerId as string, name: agentDocument.name as string, tags: agentDocument.tags as string[], status: "active", currentVersionId: null, version: 1, createdAt: new Date().toISOString(), updatedAt: new Date().toISOString() };
  store.state.agents.set(seededAgent.id, seededAgent);
  const published = await service.publishAgentVersion(publisher, workspaceId, seededAgent.id, "agent-version-key-1", { version: versionDocument });
  assert.equal(published.version.digest, versionDocument.digest);
  assert.equal(published.agent.currentVersionId, versionDocument.id);
  const versionReplay = await service.publishAgentVersion(publisher, workspaceId, seededAgent.id, "agent-version-key-1", { version: versionDocument });
  assert.equal(versionReplay.version.id, published.version.id);
  assert.equal(store.state.agents.get(seededAgent.id)?.version, 2, "idempotent replay must not bump the Agent again");

  const tamperedAgentVersion = structuredClone(versionDocument);
  tamperedAgentVersion.version = 2;
  tamperedAgentVersion.instructions = "Changed without a new digest";
  await assert.rejects(() => service.publishAgentVersion(publisher, workspaceId, seededAgent.id, "agent-version-key-2", { version: tamperedAgentVersion }), isCode("contract_violation"));
  const wrongSequence = structuredClone(versionDocument);
  await assert.rejects(() => service.publishAgentVersion(publisher, workspaceId, seededAgent.id, "agent-version-key-3", { version: wrongSequence }), isCode("agent_version_sequence_conflict"));
  await assert.rejects(() => service.publishAgentVersion(viewer, workspaceId, seededAgent.id, "agent-version-key-4", { version: versionDocument }), isCode("permission_denied"));
});

test("Agent version publication refuses unresolved profiles, digest drift, and template mismatch", async () => {
  const store = new FakeStore();
  const service = new AgentHarnessService(store);
  const agentFixture = await json(path.join(fixtures, "agent-good.json"));
  const agentDocument = agentFixture.agent as JsonDocument;
  const versionDocument = agentFixture.version as JsonDocument;
  const seededAgent: Agent = { id: agentDocument.id as string, workspaceId, ownerId: agentDocument.ownerId as string, name: agentDocument.name as string, tags: agentDocument.tags as string[], status: "active", currentVersionId: null, version: 1, createdAt: new Date().toISOString(), updatedAt: new Date().toISOString() };
  store.state.agents.set(seededAgent.id, seededAgent);
  await assert.rejects(() => service.publishAgentVersion(publisher, workspaceId, seededAgent.id, "agent-missing-profiles", { version: versionDocument }), isCode("contract_violation"));

  const bundles = await Promise.all(["hermes-profile.json", "codex-profile.json", "claude-profile.json", "generic-profile.json"].map((name) => json(path.join(fixtures, name))));
  for (const [index, bundle] of bundles.entries()) {
    await service.createHarnessDefinition(publisher, workspaceId, `definition-b-${index}`, { definition: bundle.definition as JsonDocument });
    await service.publishHarnessVersion(publisher, workspaceId, (bundle.definition as JsonDocument).id as string, `version-b-${index}`, { version: bundle.version as JsonDocument });
    await service.createHarnessProfile(publisher, workspaceId, `profile-b-${index}`, { profile: bundle.profile as JsonDocument });
  }
  await assert.rejects(() => service.publishAgentVersion(publisher, workspaceId, seededAgent.id, "agent-missing-template", { version: versionDocument }), isCode("template_version_not_found"));
  store.state.templates.set((versionDocument.template as JsonDocument).versionId as string, "c".repeat(64));
  await assert.rejects(() => service.publishAgentVersion(publisher, workspaceId, seededAgent.id, "agent-wrong-template", { version: versionDocument }), isCode("contract_violation"));
});

test("Harness profile revision enforces optimistic resourceVersion and recorded revisions", async () => {
  const store = new FakeStore();
  const service = new AgentHarnessService(store);
  const bundle = await json(path.join(fixtures, "hermes-profile.json"));
  await service.createHarnessDefinition(publisher, workspaceId, "revise-definition", { definition: bundle.definition as JsonDocument });
  await service.publishHarnessVersion(publisher, workspaceId, (bundle.definition as JsonDocument).id as string, "revise-version", { version: bundle.version as JsonDocument });
  const { profile } = await service.createHarnessProfile(publisher, workspaceId, "revise-profile", { profile: bundle.profile as JsonDocument });
  const contract = await import("./harness-contract.js");
  const revised = structuredClone(bundle.profile) as JsonDocument;
  (revised.overrides as JsonDocument)["max-turns"] = 12;
  revised.resourceVersion = 2;
  revised.digest = contract.harnessProfileDigest(revised);
  const updated = await service.reviseHarnessProfile(publisher, workspaceId, profile.id, "revise-key-1", { profile: revised, expectedResourceVersion: 1 });
  assert.equal(updated.profile.resourceVersion, 2);
  assert.notEqual(updated.profile.digest, profile.digest, "a Profile edit must change its semantic identity");
  assert.deepEqual(store.state.revisions, [{ profileId: profile.id, resourceVersion: 1 }, { profileId: profile.id, resourceVersion: 2 }]);
  await assert.rejects(() => service.reviseHarnessProfile(publisher, workspaceId, profile.id, "revise-key-2", { profile: revised, expectedResourceVersion: 1 }), isCode("profile_revision_conflict"));
});

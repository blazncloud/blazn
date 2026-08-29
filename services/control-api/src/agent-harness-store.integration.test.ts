import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { createDatabase } from "./db.js";
import { AgentHarnessService } from "./agent-harness-service.js";
import { PgAgentHarnessStore } from "./agent-harness-store.js";
import { AgentHarnessHttpError, type AgentHarnessPrincipal, type JsonDocument } from "./agent-harness-types.js";
import { harnessProfileDigest } from "./harness-contract.js";

const here = path.dirname(fileURLToPath(import.meta.url));
const fixtures = path.resolve(here, "../../../packages/contracts/testdata/harness");
const json = async (file: string) => JSON.parse(await readFile(path.join(fixtures, file), "utf8")) as Record<string, unknown>;
const runtimeUrl = process.env.BLAZN_AGENT_HARNESS_TEST_DATABASE_URL, adminUrl = process.env.BLAZN_AGENT_HARNESS_TEST_ADMIN_DATABASE_URL;

// The contract fixtures carry fixed UUIDs, so the disposable database provisions the
// fixture workspace and principals exactly once for this test run.
const workspaceId = "40000000-0000-4000-8000-000000000001";
const publisher: AgentHarnessPrincipal = { userId: "10000000-0000-4000-8000-000000000099", email: "ah-publisher@example.test", displayName: "publisher" };
const viewer: AgentHarnessPrincipal = { userId: "10000000-0000-4000-8000-000000000098", email: "ah-viewer@example.test", displayName: "viewer" };

test("PostgreSQL Agent/Harness runtime enforces tenancy, immutability, digests, and no-delete history", { skip: !runtimeUrl || !adminUrl }, async () => {
  const runtime = createDatabase(runtimeUrl!), admin = createDatabase(adminUrl!);
  const templateId = randomUUID();
  try {
    for (const user of [publisher, viewer]) await admin.query("INSERT INTO users(id,email,display_name,password_salt,password_hash) VALUES($1,$2,$3,'salt','hash')", [user.userId, user.email, user.displayName]);
    await admin.query("INSERT INTO workspaces(id,slug,name,created_by) VALUES($1,'agent-harness','Agent Harness',$2)", [workspaceId, publisher.userId]);
    await admin.query("INSERT INTO workspace_memberships(workspace_id,user_id,role) VALUES($1,$2,'owner'),($1,$3,'viewer')", [workspaceId, publisher.userId, viewer.userId]);

    const agentFixture = await json("agent-good.json");
    const versionDocument = agentFixture.version as JsonDocument;
    const template = versionDocument.template as JsonDocument;
    const templateSpec = { variants: [{ name: "linux-amd64", architecture: "amd64", imageIndex: `registry.blazn.invalid/template@sha256:${"1".repeat(64)}`, imageDigest: `registry.blazn.invalid/template@sha256:${"2".repeat(64)}`, placementProfile: "poc-linux-amd64-v1", command: ["/bin/sleep"], resources: { cpu: "1" } }] };
    const seeding = await admin.connect();
    try {
      await seeding.query("BEGIN");
      await seeding.query("INSERT INTO sandbox_templates(id,workspace_id,name,draft_revision,draft_spec,draft_digest,created_by) VALUES($1,$2,'coding-agent',1,$3::jsonb,$4,$5)", [templateId, workspaceId, JSON.stringify(templateSpec), "a".repeat(64), publisher.userId]);
      await seeding.query("INSERT INTO sandbox_template_versions(id,workspace_id,template_id,version,canonical_spec,spec,content_digest,created_by) VALUES($1,$2,$3,'poc-dev-1','{}'::bytea,$4::jsonb,$5,$6)", [template.versionId, workspaceId, templateId, JSON.stringify(templateSpec), (template.digest as string).slice("sha256:".length), publisher.userId]);
      await seeding.query("INSERT INTO sandbox_template_version_variants(version_id,workspace_id,template_id,name,architecture,image_index_digest,image_child_digest,placement_profile,command,resources) VALUES($1,$2,$3,'linux-amd64','amd64',$4,$5,'poc-linux-amd64-v1',$6::jsonb,$7::jsonb)", [template.versionId, workspaceId, templateId, `registry.blazn.invalid/template@sha256:${"1".repeat(64)}`, `registry.blazn.invalid/template@sha256:${"2".repeat(64)}`, JSON.stringify(["/bin/sleep"]), JSON.stringify({ cpu: "1" })]);
      await seeding.query("COMMIT");
    } catch (error) { await seeding.query("ROLLBACK"); throw error; } finally { seeding.release(); }

    const service = new AgentHarnessService(new PgAgentHarnessStore(runtime));
    const bundles = await Promise.all(["hermes-profile.json", "codex-profile.json", "claude-profile.json", "generic-profile.json"].map(json));
    for (const [index, bundle] of bundles.entries()) {
      const definition = bundle.definition as JsonDocument, harnessVersion = bundle.version as JsonDocument, profile = bundle.profile as JsonDocument;
      const [created, replay] = await Promise.all([
        service.createHarnessDefinition(publisher, workspaceId, `ah-definition-${index}`, { definition }),
        service.createHarnessDefinition(publisher, workspaceId, `ah-definition-${index}`, { definition }),
      ]);
      assert.equal(created.definition.id, replay.definition.id);
      await service.publishHarnessVersion(publisher, workspaceId, definition.id as string, `ah-version-${index}`, { version: harnessVersion });
      await service.createHarnessProfile(publisher, workspaceId, `ah-profile-${index}`, { profile });
    }
    await assert.rejects(() => service.createHarnessDefinition(publisher, workspaceId, "ah-definition-dup", { definition: bundles[0]!.definition as JsonDocument }), (error: unknown) => error instanceof AgentHarnessHttpError && (error.code === "definition_kind_conflict" || error.code === "identity_conflict"));

    const agentDocument = agentFixture.agent as JsonDocument;
    await admin.query("INSERT INTO agents(id,workspace_id,owner_id,name,tags,status,version,created_by) VALUES($1,$2,$3,$4,$5::jsonb,'active',1,$3)", [agentDocument.id, workspaceId, agentDocument.ownerId, agentDocument.name, JSON.stringify(agentDocument.tags)]);
    const published = await service.publishAgentVersion(publisher, workspaceId, agentDocument.id as string, "ah-agent-version-1", { version: versionDocument });
    assert.equal(published.agent.currentVersionId, versionDocument.id);
    assert.equal(published.version.version, 1);
    assert.equal((await service.getAgentVersion(viewer, workspaceId, agentDocument.id as string, versionDocument.id as string)).version.digest, versionDocument.digest);
    await assert.rejects(() => service.publishAgentVersion(viewer, workspaceId, agentDocument.id as string, "ah-agent-version-viewer", { version: versionDocument }), (error: unknown) => error instanceof AgentHarnessHttpError && error.code === "permission_denied");

    const hermesProfile = bundles[0]!.profile as JsonDocument;
    const revised = structuredClone(hermesProfile);
    (revised.overrides as JsonDocument)["max-turns"] = 8;
    revised.resourceVersion = 2;
    revised.digest = harnessProfileDigest(revised);
    const updated = await service.reviseHarnessProfile(publisher, workspaceId, hermesProfile.id as string, "ah-profile-revise-1", { profile: revised, expectedResourceVersion: 1 });
    assert.equal(updated.profile.resourceVersion, 2);
    const revisions = await admin.query("SELECT resource_version FROM harness_profile_revisions WHERE profile_id=$1 ORDER BY resource_version", [hermesProfile.id]);
    assert.deepEqual(revisions.rows.map((row) => Number(row.resource_version)), [1, 2]);

    const audits = await admin.query("SELECT event_type AS type FROM workspace_audit_events WHERE workspace_id=$1 AND (event_type LIKE 'harness.%' OR event_type LIKE 'agent.%') ORDER BY created_at", [workspaceId]);
    assert.ok(audits.rows.some((row) => row.type === "agent.version_published"));
    assert.ok(audits.rows.some((row) => row.type === "harness.profile_revised"));

    await assert.rejects(() => runtime.query("DELETE FROM agent_versions WHERE id=$1", [versionDocument.id]), pgCode("42501"));
    await assert.rejects(() => runtime.query("DELETE FROM harness_versions WHERE workspace_id=$1", [workspaceId]), pgCode("42501"));
    await assert.rejects(() => runtime.query("DELETE FROM harness_profile_revisions WHERE workspace_id=$1", [workspaceId]), pgCode("42501"));
    await assert.rejects(() => runtime.query("UPDATE agent_versions SET document='{}'::jsonb WHERE id=$1", [versionDocument.id]), pgCode("42501"));
    await assert.rejects(() => runtime.query("UPDATE harness_versions SET document='{}'::jsonb WHERE workspace_id=$1", [workspaceId]), pgCode("42501"));
    await assert.rejects(() => admin.query("INSERT INTO agent_versions(id,agent_id,workspace_id,version,digest,document,created_by) VALUES($1,$2,$3,2,$4,$5::jsonb,$6)", [randomUUID(), agentDocument.id, workspaceId, `sha256:${"f".repeat(64)}`, JSON.stringify({ id: "00000000-0000-4000-8000-00000000ffff" }), publisher.userId]), pgCode("23514"));
  } finally {
    await admin.query("DELETE FROM workspaces WHERE id=$1", [workspaceId]).catch(() => {});
    await admin.query("DELETE FROM users WHERE id=ANY($1::uuid[])", [[publisher.userId, viewer.userId]]).catch(() => {});
    await runtime.end();
    await admin.end();
  }
});
function pgCode(code: string) { return (error: unknown) => !!error && typeof error === "object" && "code" in error && (error as { code?: string }).code === code; }

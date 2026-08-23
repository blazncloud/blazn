import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createRequire } from "node:module";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import SwaggerParser from "@apidevtools/swagger-parser";
import { Ajv2020 } from "ajv/dist/2020.js";
import type { FormatsPlugin } from "ajv-formats";

const here = path.dirname(fileURLToPath(import.meta.url));
const require = createRequire(import.meta.url);
const formatsModule = require("ajv-formats") as { default?: FormatsPlugin } | FormatsPlugin;
const addFormats = ("default" in formatsModule ? formatsModule.default : formatsModule) as FormatsPlugin;
const contract = path.resolve(here, "../../../packages/contracts/projects.openapi.json");
const migration = path.resolve(here, "../migrations/010_projects.sql");
const profileMigration=path.resolve(here,"../migrations/018_project_profiles.sql");

test("Project OpenAPI is valid and exposes only workspace-scoped routes", async () => {
  const document = await SwaggerParser.validate(contract) as unknown as { paths: Record<string, Record<string, { operationId?: string }>> };
  assert.deepEqual(Object.keys(document.paths).sort(), [
    "/v1/workspaces/{workspaceId}/projects",
    "/v1/workspaces/{workspaceId}/projects/{projectId}",
    "/v1/workspaces/{workspaceId}/projects/{projectId}/profiles/{profileKind}",
  ]);
  const operations = Object.values(document.paths).flatMap((route) => Object.values(route).map((operation) => operation.operationId)).sort();
  assert.deepEqual(operations, ["createProject", "getProject", "getProjectProfile", "listProjects", "putProjectProfile", "updateProject"]);
});

test("Project profile schemas bind one ready Artifact without arbitrary plugin data", async () => {
  const document = await SwaggerParser.dereference(contract) as unknown as { components: { schemas: Record<string, object> } };
  const ajv = new Ajv2020({ strict: true, allErrors: true }); addFormats(ajv);
  const put = ajv.compile(document.components.schemas.PutProjectProfileRequest!);
  const value={schemaVersion:"blazn.content/project/v1alpha1",draftId:"00000000-0000-4000-8000-000000000004",artifactId:"00000000-0000-4000-8000-000000000005",digest:`sha256:${"a".repeat(64)}`,status:"active",expectedVersion:0};
  assert.equal(put(value),true,JSON.stringify(put.errors));
  assert.equal(put({...value,apiKey:"must-not-pass"}),false);
  assert.equal(put({...value,expectedVersion:-1}),false);
  const profile=ajv.compile(document.components.schemas.ProjectProfile!);
  const output={workspaceId:"00000000-0000-4000-8000-000000000001",projectId:"00000000-0000-4000-8000-000000000002",kind:"content",schemaVersion:value.schemaVersion,version:1,draftId:value.draftId,artifactId:value.artifactId,digest:value.digest,status:"active",createdBy:"00000000-0000-4000-8000-000000000003",updatedBy:"00000000-0000-4000-8000-000000000003",createdAt:"2026-08-23T00:00:00Z",updatedAt:"2026-08-23T00:00:00Z"};
  assert.equal(profile(output),true,JSON.stringify(profile.errors));
  assert.equal(profile({...output,providerSettings:{}}),false);
});

test("Project schemas reject secrets, unknown fields, and update no-ops", async () => {
  const document = await SwaggerParser.dereference(contract) as unknown as { components: { schemas: Record<string, object> } };
  const ajv = new Ajv2020({ strict: true, allErrors: true });
  addFormats(ajv);
  const create = ajv.compile(document.components.schemas.CreateProjectRequest!);
  const update = ajv.compile(document.components.schemas.UpdateProjectRequest!);
  assert.equal(create({ name: "Launch Video", kind: "content", description: "Campaign assets" }), true);
  assert.equal(create({ name: "Launch", apiKey: "must-not-pass" }), false);
  assert.equal(update({ expectedVersion: 1 }), false);
  assert.equal(update({ expectedVersion: 1, status: "archived" }), true);
  assert.equal(update({ expectedVersion: 1, status: "deleted" }), false);
});

test("Project response requires immutable ownership and version fields", async () => {
  const document = await SwaggerParser.dereference(contract) as unknown as { components: { schemas: Record<string, object> } };
  const ajv = new Ajv2020({ strict: true, allErrors: true });
  addFormats(ajv);
  const validate = ajv.compile(document.components.schemas.Project!);
  const project = {
    id: "00000000-0000-4000-8000-000000000002",
    workspaceId: "00000000-0000-4000-8000-000000000001",
    slug: "launch-video", kind: "content", name: "Launch Video", description: "",
    status: "active", version: 1,
    createdBy: "00000000-0000-4000-8000-000000000003",
    createdAt: "2026-08-22T00:00:00Z", updatedAt: "2026-08-22T00:00:00Z",
  };
  assert.equal(validate(project), true);
  const { workspaceId: _workspaceId, ...crossTenantAmbiguous } = project;
  assert.equal(validate(crossTenantAmbiguous), false);
});

test("Project migration enforces tenant identity, archive lifecycle, and no runtime delete", async () => {
  const sql = await readFile(migration, "utf8");
  assert.match(sql, /workspace_id uuid NOT NULL REFERENCES workspaces\(id\) ON DELETE CASCADE/);
  assert.match(sql, /UNIQUE \(workspace_id, slug\)/);
  assert.match(sql, /UNIQUE \(id, workspace_id\)/);
  assert.match(sql, /status IN \('active', 'archived'\)/);
  assert.match(sql, /version bigint NOT NULL DEFAULT 1 CHECK \(version > 0\)/);
  assert.match(sql, /GRANT SELECT, INSERT, UPDATE ON TABLE projects TO blazn_runtime/);
  assert.match(sql, /REVOKE DELETE ON TABLE projects FROM blazn_runtime/);
  assert.doesNotMatch(sql, /GRANT[^;]*DELETE[^;]*TO blazn_runtime/);
});
test("Project profile migration defers same-tenant ready Artifact integrity",async()=>{const sql=await readFile(profileMigration,"utf8");assert.match(sql,/PRIMARY KEY \(workspace_id, project_id, kind\)/);assert.match(sql,/FOREIGN KEY \(artifact_id, workspace_id, project_id\) REFERENCES artifacts\(id, workspace_id, project_id\)/);assert.match(sql,/CREATE CONSTRAINT TRIGGER project_profile_artifact_from_profile/);assert.match(sql,/CREATE CONSTRAINT TRIGGER project_profile_artifact_from_artifact/);assert.match(sql,/artifact_row\.status <> 'ready' OR artifact_row\.digest <> profile_row\.digest/);assert.match(sql,/REVOKE DELETE ON TABLE project_profiles FROM blazn_runtime/);assert.match(sql,/REVOKE ALL ON FUNCTION validate_project_profile_artifact\(\) FROM PUBLIC/);assert.doesNotMatch(sql,/GRANT[^;]*DELETE[^;]*TO blazn_runtime/);});

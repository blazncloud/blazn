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

test("Project OpenAPI is valid and exposes only workspace-scoped routes", async () => {
  const document = await SwaggerParser.validate(contract) as unknown as { paths: Record<string, Record<string, { operationId?: string }>> };
  assert.deepEqual(Object.keys(document.paths).sort(), [
    "/v1/workspaces/{workspaceId}/projects",
    "/v1/workspaces/{workspaceId}/projects/{projectId}",
  ]);
  const operations = Object.values(document.paths).flatMap((route) => Object.values(route).map((operation) => operation.operationId)).sort();
  assert.deepEqual(operations, ["createProject", "getProject", "listProjects", "updateProject"]);
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

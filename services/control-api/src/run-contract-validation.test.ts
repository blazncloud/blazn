import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createRequire } from "node:module";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import SwaggerParser from "@apidevtools/swagger-parser";
import { Ajv2020 } from "ajv/dist/2020.js";
import type { FormatsPlugin } from "ajv-formats";

const here = path.dirname(fileURLToPath(import.meta.url));
const require = createRequire(import.meta.url);
const formatsModule = require("ajv-formats") as { default?: FormatsPlugin } | FormatsPlugin;
const addFormats = ("default" in formatsModule ? formatsModule.default : formatsModule) as FormatsPlugin;
const contract = path.resolve(here, "../../../packages/contracts/runs.openapi.json");
const migration = path.resolve(here, "../migrations/012_runs_artifacts.sql");

test("Run and Artifact OpenAPI exposes only Project-scoped routes", async () => {
  const document = await SwaggerParser.validate(contract) as unknown as { paths: Record<string, Record<string, { operationId?: string }>> };
  assert.equal(Object.keys(document.paths).length, 8);
  const operations = Object.values(document.paths).flatMap((route) => Object.values(route).map((operation) => operation.operationId)).sort();
  assert.deepEqual(operations, ["cancelRun", "completeSyntheticRun", "createRun", "getArtifact", "getRun", "listArtifacts", "listRuns", "recordSyntheticRunProgress", "uploadSyntheticRunArtifact"]);
  assert.equal(Object.keys(document.paths).every((route) => route.startsWith("/v1/workspaces/{workspaceId}/projects/{projectId}/")), true);
});

test("synthetic execution schemas are closed, bounded, and cannot set authority", async () => {
  const document = await SwaggerParser.dereference(contract) as unknown as { components: { schemas: Record<string, object> }; paths: Record<string, Record<string, unknown>> };
  const ajv = new Ajv2020({ strict: true, strictRequired: false, allErrors: true }); addFormats(ajv);
  const progress = ajv.compile(document.components.schemas.SyntheticRunProgressRequest!);
  assert.equal(progress({ sequence: 0, phase: "render.plan", percent: 0, message: "ready" }), true, JSON.stringify(progress.errors));
  assert.equal(progress({ sequence: 0, phase: "render.plan", percent: 0, workspaceId: "00000000-0000-4000-8000-000000000001" }), false);
  assert.equal(progress({ sequence: 1, phase: "render", percent: 101 }), false);
  const completion = ajv.compile(document.components.schemas.CompleteSyntheticRunRequest!);
  const value = { expectedVersion: 2, planDigest: `sha256:${"a".repeat(64)}`, artifactIds: [], summary: { steps: 1, warnings: [] } };
  assert.equal(completion(value), true, JSON.stringify(completion.errors));
  assert.equal(completion({ ...value, proofClass: "local" }), false);
  const upload = ajv.compile(document.components.schemas.ArtifactUploadMetadata!);
  assert.equal(upload({ name: "preview.mp4", kind: "content.video", mediaType: "video", sizeBytes: 12, digest: `sha256:${"b".repeat(64)}` }), true, JSON.stringify(upload.errors));
  assert.equal(upload({ name: "preview.mp4", kind: "content.video", mediaType: "video", sizeBytes: 1073741825, digest: `sha256:${"b".repeat(64)}` }), false);
  const text = JSON.stringify(document.paths["/v1/workspaces/{workspaceId}/projects/{projectId}/runs/{runId}/artifacts"]);
  assert.match(text, /X-Blazn-Artifact-Metadata/);
  assert.match(text, /1073741824/);
  for (const forbidden of ["accessToken", "refreshToken", "objectKey", "signedUrl", "placement"]) assert.equal(text.includes(forbidden), false);
});

test("Run schema separates synthetic proof from populated live placement", async () => {
  const document = await SwaggerParser.dereference(contract) as unknown as { components: { schemas: Record<string, object> } };
  const ajv = new Ajv2020({ strict: true, strictRequired: false, allErrors: true }); addFormats(ajv);
  const validate = ajv.compile(document.components.schemas.Run!);
  const base = {
    id: "00000000-0000-4000-8000-000000000003", workspaceId: "00000000-0000-4000-8000-000000000001", projectId: "00000000-0000-4000-8000-000000000002",
    kind: "content.render", proofClass: "synthetic", status: "queued", version: 1, planDigest: `sha256:${"a".repeat(64)}`,
    inputArtifactIds: [], outputNames: [], requestedBy: "00000000-0000-4000-8000-000000000004", placement: null, receipt: null, createdAt: "2026-08-22T00:00:00Z",
  };
  assert.equal(validate(base), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...base, placement: { nodeId: "00000000-0000-4000-8000-000000000005" } }), false, "synthetic Run accepted live placement");
  const receipt = { schemaVersion: "blazn.run/receipt/v1alpha1", proofClass: "sandbox", outcome: "succeeded", planDigest: base.planDigest, artifactIds: [], summary: { steps: 1, warnings: [] } };
  const sandbox = { ...base, proofClass: "sandbox", status: "succeeded", placement: { nodeId: "00000000-0000-4000-8000-000000000005", sandboxId: "00000000-0000-4000-8000-000000000006" }, receipt, startedAt: "2026-08-22T00:00:01Z", completedAt: "2026-08-22T00:00:02Z" };
  assert.equal(validate(sandbox), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...sandbox, placement: null }), false, "terminal Sandbox Run accepted null placement");
  assert.equal(validate({ ...sandbox, receipt: null }), false, "terminal Run accepted null receipt");
});

test("Artifact schema exposes availability without leaking storage keys", async () => {
  const document = await SwaggerParser.dereference(contract) as unknown as { components: { schemas: Record<string, object> } };
  const ajv = new Ajv2020({ strict: true, strictRequired: false, allErrors: true }); addFormats(ajv);
  const validate = ajv.compile(document.components.schemas.Artifact!);
  const artifact = { id: "00000000-0000-4000-8000-000000000007", workspaceId: "00000000-0000-4000-8000-000000000001", projectId: "00000000-0000-4000-8000-000000000002", kind: "content.video", mediaType: "video", name: "render.mp4", status: "ready", version: 1, digest: `sha256:${"b".repeat(64)}`, sizeBytes: 10, createdBy: "00000000-0000-4000-8000-000000000004", createdAt: "2026-08-22T00:00:00Z", updatedAt: "2026-08-22T00:00:01Z", downloadAvailable: true };
  assert.equal(validate(artifact), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...artifact, objectKey: "secret/path" }), false);
  assert.equal(validate({ ...artifact, digest: undefined }), false);
  assert.equal(validate({ ...artifact, status: "pending", digest: undefined, sizeBytes: undefined, downloadAvailable: true }), false);
});

test("Run migration binds every resource to Project tenant and denies runtime delete", async () => {
  const sql = await readFile(migration, "utf8");
  assert.match(sql, /FOREIGN KEY \(project_id, workspace_id\) REFERENCES projects\(id, workspace_id\) ON DELETE CASCADE/);
  assert.match(sql, /FOREIGN KEY \(run_id, workspace_id, project_id\) REFERENCES runs\(id, workspace_id, project_id\)/);
  assert.match(sql, /FOREIGN KEY \(node_id, workspace_id\) REFERENCES nodes\(id, workspace_id\)/);
  assert.match(sql, /FOREIGN KEY \(sandbox_id, workspace_id\) REFERENCES sandboxes\(id, workspace_id\)/);
  assert.match(sql, /FOREIGN KEY \(artifact_id, workspace_id, project_id\) REFERENCES artifacts\(id, workspace_id, project_id\)/);
  assert.match(sql, /UNIQUE \(run_id, ordinal\)/);
  assert.match(sql, /run_output_names_valid\(output_names\)/);
  assert.match(sql, /CHECK \(NOT workspace_json_contains_secret_key\(receipt\)\)/);
  assert.match(sql, /CREATE CONSTRAINT TRIGGER runs_receipt_consistency_from_run/);
  assert.match(sql, /CREATE CONSTRAINT TRIGGER runs_receipt_consistency_from_receipt/);
  assert.match(sql, /terminal Run requires a receipt/);
  assert.match(sql, /Run receipt does not match terminal Run/);
  assert.match(sql, /REVOKE ALL ON FUNCTION validate_run_receipt_consistency\(\) FROM PUBLIC/);
  assert.match(sql, /REVOKE ALL ON FUNCTION run_output_names_valid\(text\[\]\) FROM PUBLIC/);
  assert.match(sql, /object_key !~ '\[\?#@\]'/);
  assert.match(sql, /REVOKE DELETE ON TABLE runs, run_events, run_receipts, artifacts, run_input_artifacts FROM blazn_runtime/);
  assert.doesNotMatch(sql, /GRANT[^;]*DELETE[^;]*TO blazn_runtime/);
});

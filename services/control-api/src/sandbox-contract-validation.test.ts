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
const contracts = path.resolve(here, "../../../packages/contracts");
const fixtures = path.join(contracts, "testdata/sandbox");
const readJSON = async (file: string): Promise<Record<string, unknown>> => JSON.parse(await readFile(file, "utf8")) as Record<string, unknown>;

test("sandbox OpenAPI is a valid dereferenceable OpenAPI 3.1 document", async () => {
  const document = await SwaggerParser.validate(path.join(contracts, "sandboxes.openapi.json")) as unknown as { openapi: string; paths?: Record<string, Record<string, unknown>> };
  assert.equal(document.openapi, "3.1.0");
  assert.equal(Object.values(document.paths ?? {}).flatMap((item) => Object.values(item ?? {})).filter((item) => typeof item === "object" && item !== null && "operationId" in item).length, 20);
});

test("actual Draft 2020-12 validation accepts good template and rejects isolated forbidden inputs", async () => {
  const schema = await readJSON(path.join(contracts, "sandbox-template.schema.json"));
  const good = await readJSON(path.join(fixtures, "template-good.json"));
  const bad = await readJSON(path.join(fixtures, "template-bad-privileged.json"));
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  const validate = ajv.compile(schema);
  assert.equal(validate(good), true, JSON.stringify(validate.errors));
  assert.equal(validate(bad), false, "bad privileged fixture unexpectedly passed actual schema validation");

  for (const forbidden of ["hostPath", "privileged", "capabilities", "serviceAccountName", "nodeSelector", "tolerations", "runtimeClassName", "secrets", "env", "volumes", "podTemplate", "unrestrictedEgress"]) {
    const candidate = structuredClone(good) as { spec: Record<string, unknown> };
    candidate.spec[forbidden] = forbidden === "privileged" ? true : {};
    assert.equal(validate(candidate), false, `forbidden input ${forbidden} unexpectedly passed`);
  }
  for (const [section, field] of [["repositories", "destination"], ["artifacts", "path"]] as const) {
    for (const segment of [".", ".."]) {
      const candidate = structuredClone(good) as { spec: Record<string, Array<Record<string, unknown>>> };
      candidate.spec[section]![0]![field] = `/workspace/${section === "repositories" ? "src" : "artifacts"}/${segment}/escape`;
      assert.equal(validate(candidate), false, `${section} accepted ${segment} segment`);
    }
  }
  for (const value of [
    `blazn/sandbox@sha256:${"a".repeat(64)}`,
    `registry.example.test:0/blazn/sandbox@sha256:${"a".repeat(64)}`,
    `registry.example.test:65536/blazn/sandbox@sha256:${"a".repeat(64)}`,
    `bad-.example.test/blazn/sandbox@sha256:${"a".repeat(64)}`,
    `registry.example.test/blazn//sandbox@sha256:${"a".repeat(64)}`,
    `registry.example.test/blazn/sandbox@sha256:${"A".repeat(64)}`,
  ]) {
    const candidate = structuredClone(good) as { spec: { variants: Array<Record<string, unknown>> } };
    candidate.spec.variants[0]!.imageDigest = value;
    assert.equal(validate(candidate), false, `non-canonical image reference ${value} unexpectedly passed`);
  }
});

test("OpenAPI immutable OCI reference agrees with the template boundary", async () => {
  const openapi = await readJSON(path.join(contracts, "sandboxes.openapi.json")) as { components: { schemas: { ImmutableOCIReference: Record<string, unknown> } } };
  const validate = new Ajv2020({ allErrors: true, strict: false }).compile(openapi.components.schemas.ImmutableOCIReference);
  const digest = `sha256:${"a".repeat(64)}`;
  assert.equal(validate(`registry.example.test/blazn/sandbox@${digest}`), true, JSON.stringify(validate.errors));
  for (const value of [`blazn/sandbox@${digest}`, `registry.example.test:0/blazn/sandbox@${digest}`, `registry.example.test/blazn//sandbox@${digest}`]) {
    assert.equal(validate(value), false, `OpenAPI accepted ${value}`);
  }
});

test("grant file header schema rejects isolated dot segments", async () => {
  const openapi = await readJSON(path.join(contracts, "sandboxes.openapi.json")) as { components: { parameters: { SandboxPath: { schema: Record<string, unknown> } } } };
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  const validate = ajv.compile(openapi.components.parameters.SandboxPath.schema);
  assert.equal(validate("/workspace/src/repo/file"), true);
  assert.equal(validate("/workspace/src/./escape"), false);
  assert.equal(validate("/workspace/src/../escape"), false);
});

test("sandbox source commit accepts only exact SHA-1 or SHA-256 widths", async () => {
  const openapi = await readJSON(path.join(contracts, "sandboxes.openapi.json")) as { components: { schemas: { SandboxSource: Record<string, unknown> } } };
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  const validate = ajv.compile(openapi.components.schemas.SandboxSource);
  for (const width of [40, 64]) assert.equal(validate({ repository: "source", commit: "a".repeat(width) }), true, JSON.stringify(validate.errors));
  for (const width of [39, 41, 63, 65]) assert.equal(validate({ repository: "source", commit: "a".repeat(width) }), false, `accepted commit width ${width}`);
});

test("semantic template and source coverage rejects duplicate identities and incomplete bindings", async () => {
  const good = await readJSON(path.join(fixtures, "template-good.json")) as { spec: { variants: Array<{ architecture: string }>; repositories: Array<{ name: string }>; artifacts: Array<{ name: string; path: string }> } };
  const unique = (values: string[]): boolean => new Set(values).size === values.length;
  assert.equal(unique(good.spec.variants.map((item) => item.architecture)), true);
  assert.equal(unique(good.spec.repositories.map((item) => item.name)), true);
  assert.equal(unique(good.spec.artifacts.map((item) => item.name)), true);
  assert.equal(unique(good.spec.artifacts.map((item) => item.path)), true);
  assert.equal(unique(["amd64", "amd64"]), false);

  const exactSources = (names: string[]): boolean => unique(names) && names.length === good.spec.repositories.length && names.every((name) => good.spec.repositories.some((repository) => repository.name === name));
  assert.equal(exactSources(["source"]), true);
  assert.equal(exactSources([]), false);
  assert.equal(exactSources(["source", "source"]), false);
  assert.equal(exactSources(["unknown"]), false);
});

test("CLI fixtures validate against exact output and requestId-preserving error schemas", async () => {
  const contract = await readJSON(path.join(contracts, "sandbox-cli-contract.json"));
  const template = await readJSON(path.join(contracts, "sandbox-template.schema.json"));
  const openapi = await readJSON(path.join(contracts, "sandboxes.openapi.json"));
  const manifest = await readJSON(path.join(fixtures, "template-good.json"));
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  ajv.addSchema(template, "https://blazn.dev/contracts/sandbox-template.schema.json");
  ajv.addSchema(openapi, "https://blazn.dev/contracts/sandboxes.openapi.json");
  ajv.addSchema(contract, "sandbox-cli");
  const error = ajv.getSchema("sandbox-cli#/errorEnvelope");
  assert.ok(error);
  assert.equal(error(await readJSON(path.join(fixtures, "cli-error.json"))), true, JSON.stringify(error.errors));

  const ids = { workspace: "11111111-1111-4111-8111-111111111111", template: "22222222-2222-4222-8222-222222222222", version: "33333333-3333-4333-8333-333333333333", sandbox: "44444444-4444-4444-8444-444444444444", operation: "55555555-5555-4555-8555-555555555555", grant: "66666666-6666-4666-8666-666666666666", actor: "77777777-7777-4777-8777-777777777777", event: "88888888-8888-4888-8888-888888888888" };
  const digest = `sha256:${"a".repeat(64)}`;
  const templateResult = { id: ids.template, workspaceId: ids.workspace, name: "coding-small", draftVersion: 1, publishedVersionId: ids.version, createdAt: "2026-08-22T00:00:00Z", updatedAt: "2026-08-22T00:00:00Z" };
  const versionResult = { id: ids.version, workspaceId: ids.workspace, templateId: ids.template, name: "coding-small", version: "bootstrap-1", contentDigest: digest, manifest, status: "published", createdAt: "2026-08-22T00:00:00Z" };
  const sandbox = { id: ids.sandbox, workspaceId: ids.workspace, requestedBy: ids.actor, templateId: ids.template, templateVersionId: ids.version, templateName: "coding-small", templateVersion: "bootstrap-1", templateDigest: digest, variantName: "linux-amd64", imageIndexDigest: `registry.invalid/poc@${digest}`, imageDigest: `registry.invalid/poc@sha256:${"b".repeat(64)}`, architecture: "amd64", allocationMode: "direct", sourceBindings: [{ repository: "source", url: "https://github.com/blazncloud/blazn.git", destination: "/workspace/src/blazn", writable: true, commit: "1".repeat(40) }], artifactContract: { digest, items: [{ name: "patch", path: "/workspace/artifacts/change.patch", mediaType: "text/plain", required: true }] }, state: "requested", desiredState: "ready", version: 1, queueName: "poc-local", admissionId: null, isolation: "approved-non-sensitive-poc", expiresAt: "2026-08-22T00:15:00Z", conditions: [], createdAt: "2026-08-22T00:00:00Z", updatedAt: "2026-08-22T00:00:00Z" };
  const pendingOperation = (type: "create" | "stop" | "delete") => ({ id: ids.operation, sandboxId: ids.sandbox, type, status: "pending", expectedSandboxVersion: 1, receipt: null, createdAt: "2026-08-22T00:00:00Z", completedAt: null });
  const event = { eventId: ids.event, sandboxId: ids.sandbox, operationId: ids.operation, sequence: 0, type: "sandbox.requested", payload: {}, createdAt: "2026-08-22T00:00:00Z" };
  const transfer = { sandboxId: ids.sandbox, grantId: ids.grant, source: "/tmp/source", destination: "/workspace/src/blazn/source", size: 2, sha256: digest };
  const outputs: Record<string, unknown> = {
    "template validate": { valid: true, manifestDigest: digest, errors: [], warnings: [] },
    "template publish": { template: templateResult, version: versionResult },
    "sandbox create": { sandbox, operation: pendingOperation("create") },
    "sandbox list": { items: [sandbox], nextCursor: null },
    "sandbox get": sandbox,
    "sandbox watch": event,
    "sandbox exec": await readJSON(path.join(fixtures, "cli-exec-success.json")),
    "sandbox upload": transfer,
    "sandbox download": transfer,
    "sandbox stop": { sandbox, operation: pendingOperation("stop") },
    "sandbox delete": { sandbox, operation: pendingOperation("delete") },
  };
  const commands = contract.commands as Record<string, { outputSchema?: { $ref: string } }>;
  for (const [command, output] of Object.entries(outputs)) {
    const ref = commands[command]?.outputSchema?.$ref;
    assert.ok(ref, `missing output schema for ${command}`);
    const validate = ajv.getSchema(`sandbox-cli${ref}`);
    assert.ok(validate, `unresolved output schema for ${command}`);
    assert.equal(validate(output), true, `${command}: ${JSON.stringify(validate.errors)}`);
  }

  const createReceipt = { id: ids.event, operationId: ids.operation, operationType: "create", status: "succeeded", cleanupComplete: false, artifactExportComplete: false, grantsRevoked: false, backendDestroyed: false, backend: { present: true, uid: "backend-uid", resourceVersion: "1" }, result: null, error: null, createdAt: "2026-08-22T00:00:01Z" };
  const stopReceipt = { ...createReceipt, operationType: "stop", cleanupComplete: true, artifactExportComplete: true, grantsRevoked: true, backendDestroyed: true, backend: { present: false, uid: null, resourceVersion: null }, result: { artifactIds: [], warnings: [] } };
  const receipt = ajv.getSchema("sandbox-cli#/$defs/receipt"); assert.ok(receipt);
  assert.equal(receipt(createReceipt), true, JSON.stringify(receipt.errors));
  assert.equal(receipt(stopReceipt), true, JSON.stringify(receipt.errors));
  assert.equal(receipt({ ...stopReceipt, cleanupComplete: false }), false, "incomplete successful stop receipt passed");
  const operation = ajv.getSchema("sandbox-cli#/$defs/operation"); assert.ok(operation);
  const completedStop = { ...pendingOperation("stop"), status: "succeeded", receipt: stopReceipt, completedAt: "2026-08-22T00:00:01Z" };
  const completedCreate = { ...pendingOperation("create"), status: "succeeded", receipt: createReceipt, completedAt: "2026-08-22T00:00:01Z" };
  assert.equal(operation(completedStop), true, JSON.stringify(operation.errors));
  assert.equal(operation(completedCreate), true, JSON.stringify(operation.errors));
  assert.equal(operation({ ...completedStop, receipt: { ...stopReceipt, status: "failed" } }), false, "operation accepted mismatched receipt status");
  assert.equal(operation({ ...pendingOperation("delete"), receipt: stopReceipt }), false, "nonterminal operation accepted a receipt");
});

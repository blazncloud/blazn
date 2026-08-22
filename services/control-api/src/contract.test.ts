import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { API_ERROR_STATUS } from "./contract.js";

interface OpenApiDocument {
  paths: Record<string, Record<string, { requestBody?: { content: { "application/json": { schema: { $ref: string } } } }; responses?: Record<string, unknown> }>>;
  components: { schemas: { Error: { properties: { code: { enum: string[] } }; "x-blazn-error-status": Record<string, number> } } };
}

interface ServerConformance {
  errorStatus: Record<string, number>;
  requiredOperations: Record<string, string>;
  proofBoundRevokeRequest: string;
}

const here = path.dirname(fileURLToPath(import.meta.url));

async function conformance(): Promise<ServerConformance> {
  return JSON.parse(await readFile(path.resolve(here, "../contract/server-conformance.json"), "utf8")) as ServerConformance;
}

async function openApiIfPresent(): Promise<OpenApiDocument | undefined> {
  try {
    return JSON.parse(await readFile(path.resolve(here, "../../../packages/contracts/openapi.json"), "utf8")) as OpenApiDocument;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return undefined;
    throw error;
  }
}

function sortedRecord(value: Record<string, number>): Record<string, number> {
  return Object.fromEntries(Object.entries(value).sort(([left], [right]) => left.localeCompare(right)));
}

test("machine-readable error codes and statuses exactly match the server registry", async () => {
  const manifest = await conformance();
  const expected = sortedRecord(API_ERROR_STATUS);
  assert.deepEqual(sortedRecord(manifest.errorStatus), expected);
  const document = await openApiIfPresent();
  if (document) {
    const schema = document.components.schemas.Error;
    assert.deepEqual(sortedRecord(schema["x-blazn-error-status"]), expected);
    assert.deepEqual([...schema.properties.code.enum].sort(), Object.keys(expected));
  }
});

test("published operations have default errors and proof-bound revocation", async (context) => {
  const manifest = await conformance();
  const document = await openApiIfPresent();
  if (!document) {
    context.skip("repository OpenAPI is outside the pinned service-only test archive");
    return;
  }
  for (const [route, method] of Object.entries(manifest.requiredOperations)) {
    const operation = document.paths[route]?.[method];
    assert.ok(operation, `${method.toUpperCase()} ${route} is absent`);
    assert.ok(operation.responses?.default, `${method.toUpperCase()} ${route} has no default error response`);
  }
  assert.equal(document.paths["/v1/auth/sessions/revoke"]?.post?.requestBody?.content["application/json"].schema.$ref, manifest.proofBoundRevokeRequest);
});

import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { API_ERROR_STATUS } from "./contract.js";

interface OpenApiDocument {
  paths: Record<string, Record<string, { responses?: Record<string, unknown> }>>;
  components: {
    schemas: {
      Error: {
        properties: { code: { enum: string[] } };
        "x-blazn-error-status": Record<string, number>;
      };
    };
  };
}

async function openApi(): Promise<OpenApiDocument> {
  const here = path.dirname(fileURLToPath(import.meta.url));
  const source = path.resolve(here, "../../../packages/contracts/openapi.json");
  return JSON.parse(await readFile(source, "utf8")) as OpenApiDocument;
}

test("OpenAPI error codes and statuses exactly match the server registry", async () => {
  const document = await openApi();
  const schema = document.components.schemas.Error;
  const expected = Object.fromEntries(Object.entries(API_ERROR_STATUS).sort(([left], [right]) => left.localeCompare(right)));
  const declared = Object.fromEntries(Object.entries(schema["x-blazn-error-status"]).sort(([left], [right]) => left.localeCompare(right)));
  assert.deepEqual(declared, expected);
  assert.deepEqual([...schema.properties.code.enum].sort(), Object.keys(expected));
});

test("every published operation has a machine-readable unexpected-error response", async () => {
  const document = await openApi();
  for (const [route, pathItem] of Object.entries(document.paths)) {
    for (const [method, operation] of Object.entries(pathItem)) {
      assert.ok(operation.responses?.default, `${method.toUpperCase()} ${route} has no default error response`);
    }
  }
});

test("proof-bound revocation publishes the refresh request contract", async () => {
  const document = await openApi() as unknown as Record<string, unknown>;
  const operation = (((document.paths as Record<string, unknown>)["/v1/auth/sessions/revoke"] as Record<string, unknown>).post as Record<string, unknown>);
  const requestBody = operation.requestBody as Record<string, unknown>;
  const content = requestBody.content as Record<string, unknown>;
  const json = content["application/json"] as Record<string, unknown>;
  assert.equal((json.schema as Record<string, unknown>).$ref, "#/components/schemas/RefreshSessionRequest");
});

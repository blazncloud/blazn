import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { NODE_ERROR_STATUS, nodeErrorBody, NodeHttpError } from "./node-types.js";

interface NodeContract {
  components: {
    schemas: { NodeError: { required: string[]; properties: { code: { enum: string[] } }; "x-blazn-error-status": Record<string, number> } };
    responses: { Error: { content: { "application/json": { schema: { $ref: string } } } } };
    securitySchemes: Record<string, unknown>;
  };
}

const here=path.dirname(fileURLToPath(import.meta.url));
function sorted<T>(value:Record<string,T>):Record<string,T>{return Object.fromEntries(Object.entries(value).sort(([a],[b])=>a.localeCompare(b)));}

test("Node error registry and HTTP envelope exactly match the frozen NodeError contract",async()=>{
  const document=JSON.parse(await readFile(path.resolve(here,"../../../packages/contracts/nodes.openapi.json"),"utf8")) as NodeContract;
  const schema=document.components.schemas.NodeError;
  assert.deepEqual(sorted(NODE_ERROR_STATUS),sorted(schema["x-blazn-error-status"]));
  assert.deepEqual(Object.keys(NODE_ERROR_STATUS).sort(),[...schema.properties.code.enum].sort());
  assert.deepEqual([...schema.required].sort(),["code","message","requestId"]);
  assert.deepEqual(Object.keys(nodeErrorBody(new NodeHttpError("identity_rejected","rejected"),"request-1")).sort(),["code","message","requestId"]);
  assert.equal(new NodeHttpError("identity_rejected","rejected").status,403);
  assert.equal(document.components.responses.Error.content["application/json"].schema.$ref,"#/components/schemas/NodeError");
  assert.ok(document.components.securitySchemes.bearerAuth);assert.ok(document.components.securitySchemes.nodeProof);
});

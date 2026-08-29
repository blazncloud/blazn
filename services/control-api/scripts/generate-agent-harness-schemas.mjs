#!/usr/bin/env node
// Embeds the normative Agent and Harness JSON Schemas into a compiled module so the
// control API can validate publication requests at runtime without filesystem access
// to packages/contracts. Regenerate with: node scripts/generate-agent-harness-schemas.mjs
// The agent-harness-schemas sync test fails when this file is stale.
import { createHash } from "node:crypto";
import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const contracts = path.resolve(here, "../../../packages/contracts");
const target = path.resolve(here, "../src/agent-harness-schemas.generated.ts");

const agentSource = await readFile(path.join(contracts, "agent.schema.json"), "utf8");
const harnessSource = await readFile(path.join(contracts, "harness.schema.json"), "utf8");
const agentSchema = JSON.parse(agentSource);
const harnessSchema = JSON.parse(harnessSource);

const extract = (schema, property) => {
  const part = structuredClone(schema.properties[property]);
  part.$defs = structuredClone(schema.$defs);
  delete part.$id;
  return part;
};
const strip = (schema) => {
  const copy = structuredClone(schema);
  delete copy.$id;
  return copy;
};
const sha256 = (value) => createHash("sha256").update(value).digest("hex");

const body = `// GENERATED FILE - DO NOT EDIT.
// Source: packages/contracts/agent.schema.json, packages/contracts/harness.schema.json
// Regenerate with: node scripts/generate-agent-harness-schemas.mjs
export const agentSchemaSourceSha256 = ${JSON.stringify(sha256(agentSource))};
export const harnessSchemaSourceSha256 = ${JSON.stringify(sha256(harnessSource))};
export const agentBundleSchema = ${JSON.stringify(strip(agentSchema))} as const;
export const harnessBundleSchema = ${JSON.stringify(strip(harnessSchema))} as const;
export const harnessDefinitionSchema = ${JSON.stringify(extract(harnessSchema, "definition"))} as const;
export const harnessVersionSchema = ${JSON.stringify(extract(harnessSchema, "version"))} as const;
export const harnessProfileSchema = ${JSON.stringify(extract(harnessSchema, "profile"))} as const;
`;

await writeFile(target, body);
process.stdout.write(`wrote ${path.relative(process.cwd(), target)}\n`);

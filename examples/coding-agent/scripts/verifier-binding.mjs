import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const example = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const repository = path.resolve(example, "../..");
const sha256 = (value) => `sha256:${createHash("sha256").update(value).digest("hex")}`;
export function verifierIdentity(source, schema, compiled) { return {schemaVersion:"blazn.dev/example-verifier-binding/v1alpha1",sourceDigest:sha256(source),schemaDigest:sha256(schema),compiledDigest:sha256(compiled)}; }
export function assertVerifierIdentity(expected, source, schema, compiled) { assert.deepEqual(verifierIdentity(source,schema,compiled),expected,"compiled Development verifier is stale or substituted"); }
export async function verifyRepositoryBinding() {
  const expected=JSON.parse(await readFile(path.join(example,"fixtures/verifier-binding.json"),"utf8"));
  const service=path.join(repository,"services/control-api");
  assertVerifierIdentity(expected,await readFile(path.join(service,"src/development-contract.ts")),await readFile(path.join(repository,"packages/contracts/development-project.schema.json")),await readFile(path.join(service,"dist/development-contract.js")));
}
if(process.argv[1]&&path.resolve(process.argv[1])===fileURLToPath(import.meta.url))verifyRepositoryBinding().catch(error=>{process.stderr.write(`${error.message}\n`);process.exitCode=1;});

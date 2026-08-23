import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { createRequire } from "node:module";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const example = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const repository = path.resolve(example, "../..");
const service = path.join(repository, "services/control-api");
const require = createRequire(path.join(service, "package.json"));
const { default: Ajv2020 } = require("ajv/dist/2020.js");
const formatsModule = require("ajv-formats");
const addFormats = formatsModule.default ?? formatsModule;
const { verifyDevelopmentProjectCommands } = await import(pathToFileURL(path.join(service, "dist/development-contract.js")).href);

const json = async (name) => JSON.parse(await readFile(path.join(example, name), "utf8"));
const sha256 = (value) => `sha256:${createHash("sha256").update(value).digest("hex")}`;
const canonical = (value) => {
  if (value === null || typeof value !== "object") return JSON.stringify(value);
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
  return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${canonical(value[key])}`).join(",")}}`;
};
const contextFiles = [
  ".dockerignore",
  "Dockerfile",
  "fixtures/source/calculator.mjs",
  "fixtures/task.json",
  "package-lock.json",
  "package.json",
  "src/coding-agent.mjs",
  "test/coding-agent.test.mjs"
];

async function buildContextDigest() {
  const files = {};
  for (const name of [...contextFiles].sort()) files[name] = sha256(await readFile(path.join(example, name)));
  return sha256(`blazn-example-build-context-v1\n${canonical(files)}`);
}

function setPath(value, dottedPath, replacement) {
  const parts = dottedPath.split(".");
  let cursor = value;
  for (const part of parts.slice(0, -1)) cursor = cursor[part];
  cursor[parts.at(-1)] = replacement;
}

function exampleSemanticErrors(project) {
  const errors = [...verifyDevelopmentProjectCommands(project)];
  const expectedPolicy = {
    builderProfile: "trusted-buildkit-v1",
    networkProfile: "build-egress-v1",
    resourceProfile: "poc-build-small-v1",
    publicationPolicy: "poc-development-v1"
  };
  if (canonical(project.policy) !== canonical(expectedPolicy)) errors.push("example policy is not approved");
  if (canonical(project.platforms) !== canonical(["linux/amd64", "linux/arm64"])) errors.push("example platform order is not frozen");
  return errors;
}

const manifest = await json("blazn.project.json");
const schema = JSON.parse(await readFile(path.join(repository, "packages/contracts/development-project.schema.json"), "utf8"));
const ajv = new Ajv2020({ allErrors: true, strict: false });
addFormats(ajv);
const validate = ajv.compile(schema);
assert.equal(validate(manifest), true, JSON.stringify(validate.errors));
assert.deepEqual(exampleSemanticErrors(manifest), []);

const lockDigest = sha256(await readFile(path.join(example, "package-lock.json")));
assert.equal(manifest.dependencyLocks["examples/coding-agent/package-lock.json"], lockDigest);
const identities = await json("fixtures/identities.json");
assert.equal(identities.dependencyLockDigest, lockDigest);
assert.equal(identities.buildContextDigest, await buildContextDigest());
assert.equal(await buildContextDigest(), await buildContextDigest());

const baseImage = await json("fixtures/base-image.json");
assert.equal(`${baseImage.repository}@${baseImage.indexDigest}`, identities.baseImageDigest);
assert.deepEqual(baseImage.manifests.map((item) => item.platform), ["linux/amd64", "linux/arm64"]);
for (const item of baseImage.manifests) assert.match(item.digest, /^sha256:[0-9a-f]{64}$/);

const dockerfile = await readFile(path.join(example, "Dockerfile"), "utf8");
const from = dockerfile.split("\n").filter((line) => line.startsWith("FROM "));
assert.equal(from.length, 2);
for (const line of from) {
  assert.match(line, /^FROM docker\.io\/library\/node@sha256:[0-9a-f]{64}(?: AS [a-z]+)?$/);
  assert.equal(line.split(" ")[1], identities.baseImageDigest);
}
assert.doesNotMatch(dockerfile, /(?::latest|--mount=type=secret|https?:\/\/[^/\s]+@)/i);

const forbiddenKeys = new Set(["token", "password", "secret", "credential", "authorization", "apikey", "privatekey", "kubeconfig"]);
const scan = (value) => {
  if (Array.isArray(value)) return value.flatMap(scan);
  if (value !== null && typeof value === "object") return Object.entries(value).flatMap(([key, child]) => [forbiddenKeys.has(key.toLowerCase().replace(/[^a-z0-9]/g, "")) ? key : "", ...scan(child)]).filter(Boolean);
  if (typeof value === "string" && /(?:^|[?&])(?:api[_-]?key|token|password|secret|authorization)=|:\/\/[^/\s]+@|:latest\b/i.test(value)) return [value];
  return [];
};
assert.deepEqual(scan(manifest), []);
assert.deepEqual(scan(await json("fixtures/task.json")), []);

for (const fixture of await json("fixtures/invalid-projects.json")) {
  const candidate = structuredClone(manifest);
  for (const [key, value] of Object.entries(fixture.set)) setPath(candidate, key, value);
  if (fixture.layer === "schema") assert.equal(validate(candidate), false, `${fixture.name} passed schema validation`);
  else {
    assert.equal(validate(candidate), true, `${fixture.name} did not reach semantic validation`);
    assert.notDeepEqual(exampleSemanticErrors(candidate), [], `${fixture.name} passed semantic validation`);
  }
}

process.stdout.write(`development-project: valid\nlock: ${lockDigest}\nbuild-context: ${identities.buildContextDigest}\nnegative-fixtures: passed\n`);

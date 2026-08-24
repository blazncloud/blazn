import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { createRequire } from "node:module";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { buildContextIdentity } from "./context-identity.mjs";
import { verifyRepositoryBinding } from "./verifier-binding.mjs";

const example = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const repository = path.resolve(example, "../..");
const service = path.join(repository, "services/control-api");
const require = createRequire(path.join(service, "package.json"));
const { default: Ajv2020 } = require("ajv/dist/2020.js");
const formatsModule = require("ajv-formats");
const addFormats = formatsModule.default ?? formatsModule;
await verifyRepositoryBinding();
const { developmentDigest, verifyDevelopmentProjectCommands } = await import(pathToFileURL(path.join(service, "dist/development-contract.js")).href);

const json = async (name) => JSON.parse(await readFile(path.join(example, name), "utf8"));
const sha256 = (value) => `sha256:${createHash("sha256").update(value).digest("hex")}`;
const canonical = (value) => {
  if (value === null || typeof value !== "object") return JSON.stringify(value);
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
  return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${canonical(value[key])}`).join(",")}}`;
};
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
  if (project.build?.context !== "examples/coding-agent") errors.push("example build context is not frozen");
  if (project.build?.dockerfile !== "examples/coding-agent/Dockerfile") errors.push("example Dockerfile is not frozen");
  return errors;
}

function crossResourceErrors(project, template, agent) {
  const errors=[];
  if (template.metadata?.name!=="coding-agent" || agent.metadata?.name!==template.metadata?.name) errors.push("Agent/template name mismatch");
  if (template.spec?.repositories?.length!==1 || template.spec.repositories[0]?.url!==project.repository?.url) errors.push("Project/template repository mismatch");
  if (agent.spec?.repository?.url!==project.repository?.url) errors.push("Project/Agent repository mismatch");
  if (agent.spec?.resourceProfile!==project.policy?.resourceProfile) errors.push("Project/Agent resource profile mismatch");
  if (canonical(template.spec?.artifacts)!==canonical([{name:"patch",path:"/workspace/artifacts/change.patch",mediaType:"text/plain",required:true}])) errors.push("patch Artifact contract mismatch");
  if (agent.spec?.instructions!=="Apply one deterministic source edit and return the resulting patch as an Artifact.") errors.push("Agent patch intent mismatch");
  return errors;
}

const manifest = await json("blazn.yaml");
const schema = JSON.parse(await readFile(path.join(repository, "packages/contracts/development-project.schema.json"), "utf8"));
const templateSchema = JSON.parse(await readFile(path.join(repository, "packages/contracts/sandbox-template.schema.json"), "utf8"));
const ajv = new Ajv2020({ allErrors: true, strict: false });
addFormats(ajv);
const validate = ajv.compile(schema);
const validateTemplate = ajv.compile(templateSchema);
assert.equal(validate(manifest), true, JSON.stringify(validate.errors));
assert.deepEqual(exampleSemanticErrors(manifest), []);

const template = await json("sandbox-template.yaml");
assert.equal(validateTemplate(template),true,JSON.stringify(validateTemplate.errors));
const templateDigest=developmentDigest(template.spec);
assert.deepEqual(manifest.template,{versionId:"60000000-0000-4000-8000-000000000006",digest:templateDigest});
assert.deepEqual(manifest.publicationTarget,{templateId:"50000000-0000-4000-8000-000000000006"});
const agent=await json("agent.yaml"),agentKeys=["allowedHarnessProfiles","defaultHarnessProfile","instructions","modelPolicy","objective","projectId","repository","resourceProfile","sandboxTemplate","tools","version"];
assert.deepEqual(Object.keys(agent).sort(),["apiVersion","kind","metadata","spec"]);
assert.equal(agent.apiVersion,"blazn.dev/v1alpha1");assert.equal(agent.kind,"Agent");assert.deepEqual(Object.keys(agent.metadata).sort(),["id","name"]);assert.match(agent.metadata.id,/^[0-9a-f-]{36}$/);assert.equal(agent.metadata.name,"coding-agent");assert.deepEqual(Object.keys(agent.spec).sort(),agentKeys.sort());
assert.equal(agent.spec.projectId,manifest.projectId);assert.deepEqual(agent.spec.repository,manifest.repository);assert.deepEqual(agent.spec.sandboxTemplate,manifest.template);assert.deepEqual(agent.spec.allowedHarnessProfiles,["hermes","codex-cli","claude-code"]);assert.ok(agent.spec.allowedHarnessProfiles.includes(agent.spec.defaultHarnessProfile));assert.deepEqual(agent.spec.tools,[]);
assert.deepEqual(crossResourceErrors(manifest,template,agent),[]);
for(const mutate of [
  (p,t,a)=>{p.repository.url="https://example.test/substituted.git";},
  (p,t,a)=>{t.metadata.name="substituted";},
  (p,t,a)=>{a.spec.resourceProfile="substituted-profile";},
  (p,t,a)=>{t.spec.artifacts[0].path="/workspace/artifacts/substituted.patch";}
]){const p=structuredClone(manifest),t=structuredClone(template),a=structuredClone(agent);mutate(p,t,a);assert.notDeepEqual(crossResourceErrors(p,t,a),[]);}
const identities=await json("fixtures/identities.json"),agentDigest=developmentDigest(agent.spec);
assert.deepEqual({projectId:identities.projectId,templateId:identities.templateId,templateVersionId:identities.templateVersionId,templateDigest:identities.templateDigest,agentId:identities.agentId,agentVersionId:identities.agentVersionId,agentDigest:identities.agentDigest},{projectId:manifest.projectId,templateId:manifest.publicationTarget.templateId,templateVersionId:manifest.template.versionId,templateDigest,agentId:agent.metadata.id,agentVersionId:"71000000-0000-4000-8000-000000000006",agentDigest});

const lockDigest = sha256(await readFile(path.join(example, "package-lock.json")));
assert.equal(manifest.dependencyLocks["examples/coding-agent/package-lock.json"], lockDigest);
assert.equal(identities.dependencyLockDigest, lockDigest);
assert.equal(identities.buildContextDigest, await buildContextIdentity(example));
assert.equal(await buildContextIdentity(example), await buildContextIdentity(example));

const baseImage = await json("fixtures/base-image.json");
assert.equal(`${baseImage.repository}@${baseImage.indexDigest}`, identities.baseImageDigest);
assert.deepEqual(baseImage.manifests.map((item) => item.platform), ["linux/amd64", "linux/arm64"]);
for (const item of baseImage.manifests) assert.match(item.digest, /^sha256:[0-9a-f]{64}$/);
assert.deepEqual(template.spec.variants.map((variant)=>variant.architecture),["amd64","arm64"]);
for(const variant of template.spec.variants){const platform=`linux/${variant.architecture}`,child=baseImage.manifests.find(item=>item.platform===platform);assert.equal(variant.imageIndex,identities.baseImageDigest);assert.equal(variant.imageDigest,`${baseImage.repository}@${child.digest}`);}

const dockerfile = await readFile(path.join(example, "Dockerfile"), "utf8");
const from = dockerfile.split("\n").filter((line) => line.startsWith("FROM "));
assert.equal(from.length, 2);
for (const line of from) {
  assert.match(line, /^FROM docker\.io\/library\/node@sha256:[0-9a-f]{64}(?: AS [a-z]+)?$/);
  assert.equal(line.split(" ")[1], identities.baseImageDigest);
}
assert.doesNotMatch(dockerfile, /(?::latest|--mount=type=secret|https?:\/\/[^/\s]+@)/i);
assert.match(dockerfile, /CMD \["--task", "\/workspace\/src\/blazn\/examples\/coding-agent\/fixtures\/task\.json", "--source-root", "\/workspace\/src\/blazn", "--output", "\/workspace\/artifacts\/change\.patch"\]/);

const forbiddenKeys = new Set(["token", "password", "secret", "credential", "authorization", "apikey", "privatekey", "kubeconfig"]);
const scan = (value) => {
  if (Array.isArray(value)) return value.flatMap(scan);
  if (value !== null && typeof value === "object") return Object.entries(value).flatMap(([key, child]) => [forbiddenKeys.has(key.toLowerCase().replace(/[^a-z0-9]/g, "")) ? key : "", ...scan(child)]).filter(Boolean);
  if (typeof value === "string" && /(?:^|[?&])(?:api[_-]?key|token|password|secret|authorization)=|:\/\/[^/\s]+@|:latest\b/i.test(value)) return [value];
  return [];
};
assert.deepEqual(scan(manifest), []);
assert.deepEqual(scan(template), []);
assert.deepEqual(scan(agent), []);
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

process.stdout.write(`development-project: valid\ntemplate: ${templateDigest}\nagent: ${agentDigest}\nlock: ${lockDigest}\nbuild-context: ${identities.buildContextDigest}\nnegative-fixtures: passed\n`);

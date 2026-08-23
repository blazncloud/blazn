import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createRequire } from "node:module";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { Ajv2020 } from "ajv/dist/2020.js";
import type { FormatsPlugin } from "ajv-formats";
import { developmentBuildInputDigest, developmentDigest, developmentRefreshCacheKey, developmentRefreshInputsDigest, verifyDevelopmentFinalization, verifyDevelopmentProjectCommands } from "./development-contract.js";

const here = path.dirname(fileURLToPath(import.meta.url));
const contracts = path.resolve(here, "../../../packages/contracts");
const fixtures = path.join(contracts, "testdata/development");
const require = createRequire(import.meta.url);
const formatsModule = require("ajv-formats") as { default?: FormatsPlugin } | FormatsPlugin;
const addFormats = ("default" in formatsModule ? formatsModule.default : formatsModule) as FormatsPlugin;
const readJSON = async (file: string): Promise<Record<string, unknown>> => JSON.parse(await readFile(file, "utf8")) as Record<string, unknown>;

function validator(schema: Record<string, unknown>) {
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  return ajv.compile(schema);
}

test("DevelopmentProject accepts the POC declaration and rejects narrow unsafe fixtures", async () => {
  const validate = validator(await readJSON(path.join(contracts, "development-project.schema.json")));
  assert.equal(validate(await readJSON(path.join(fixtures, "project-good.json"))), true, JSON.stringify(validate.errors));
  for (const name of ["project-bad-secret.json", "project-bad-path.json", "project-bad-mutable-output.json"]) {
    assert.equal(validate(await readJSON(path.join(fixtures, name))), false, `${name} unexpectedly passed`);
  }
});

test("DevelopmentProject freezes exact platforms, lock digests, paths, and committed argv", async () => {
  const validate = validator(await readJSON(path.join(contracts, "development-project.schema.json")));
  const good = await readJSON(path.join(fixtures, "project-good.json"));
  const cases: Array<[string, (value: Record<string, unknown>) => void]> = [
    ["missing ARM64", (value) => { value.platforms = ["linux/amd64"]; }],
    ["mutable lock", (value) => { (value.dependencyLocks as Record<string, unknown>)["package-lock.json"] = "latest"; }],
    ["absolute lock path", (value) => { const locks = value.dependencyLocks as Record<string, unknown>; locks["/etc/passwd"] = locks["package-lock.json"]; delete locks["package-lock.json"]; }],
    ["shell string", (value) => { ((value.tests as Record<string, Record<string, unknown>>).poc!).argv = "npm test"; }],
    ["inline environment", (value) => { ((value.tests as Record<string, Record<string, unknown>>).poc!).env = { TOKEN: "forbidden" }; }],
  ];
  for (const [label, mutate] of cases) {
    const candidate = structuredClone(good);
    mutate(candidate);
    assert.equal(validate(candidate), false, `${label} unexpectedly passed`);
  }
});

test("committed test commands reject direct shells and embedded credentials", async () => {
  const good = await readJSON(path.join(fixtures, "project-good.json"));
  assert.deepEqual(verifyDevelopmentProjectCommands(good), []);
  for (const argv of [["sh", "-c", "npm test"], ["/usr/bin/env", "npm", "test"], ["npm", "test", "--api-key=forbidden"], ["npm", "test", "OPENAI_API_KEY=forbidden"], ["npm", "test", "--header", "Authorization: Basic forbidden"], ["npm", "test", "--header", "Authorization", "Basic", "forbidden"], ["npm", "test", "-H", "X-Api-Key", "forbidden"], ["npm", "test", "-HX-Api-Key: forbidden"], ["npm", "test", "https://example.invalid/test?api_key=forbidden"], ["npm", "test", "https://example.invalid/test?%252561pi_key=forbidden"], ["npm", "test", "https://token@example.invalid/pkg"], ["npm", "test", "x".repeat(4097)]]) {
    const candidate = structuredClone(good);
    ((candidate.tests as Record<string, Record<string, unknown>>).poc!).argv = argv;
    assert.notDeepEqual(verifyDevelopmentProjectCommands(candidate), [], `accepted unsafe argv ${JSON.stringify(argv)}`);
  }
});

test("Build accepts controller evidence and rejects mutable source or leaked authority", async () => {
  const validate = validator(await readJSON(path.join(contracts, "development-build.schema.json")));
  assert.equal(validate(await readJSON(path.join(fixtures, "build-good.json"))), true, JSON.stringify(validate.errors));
  for (const name of ["build-bad-mutable-source.json", "build-bad-authority.json"]) {
    assert.equal(validate(await readJSON(path.join(fixtures, name))), false, `${name} unexpectedly passed`);
  }
});

test("publication is fail closed on architecture, digests, scans, tests, and reproducibility", async () => {
  const validate = validator(await readJSON(path.join(contracts, "development-build.schema.json")));
  const good = await readJSON(path.join(fixtures, "build-good.json"));
  const cases: Array<[string, (value: Record<string, unknown>) => void]> = [
    ["duplicate architecture output", (value) => { const outputs = value.outputs as { images: Array<Record<string, unknown>> }; outputs.images[1]!.platform = "linux/amd64"; }],
    ["mutable output", (value) => { const outputs = value.outputs as { imageIndexDigest: string }; outputs.imageIndexDigest = "registry.blazn.invalid/poc/coding-agent:latest"; }],
    ["secret finding", (value) => { const evidence = value.evidence as { secretScan: Record<string, unknown> }; evidence.secretScan = { passed: false, findings: 1 }; }],
    ["failed security test", (value) => { const evidence = value.evidence as { securityTests: Array<Record<string, unknown>> }; evidence.securityTests[0]!.passed = false; }],
    ["failed lifecycle test", (value) => { const evidence = value.evidence as { lifecycleTests: Array<Record<string, unknown>> }; evidence.lifecycleTests[1]!.passed = false; }],
    ["unexplained nondeterminism", (value) => { const evidence = value.evidence as { reproducibility: Record<string, unknown> }; evidence.reproducibility = { outcome: "explained-nondeterminism" }; }],
    ["failed cleanup", (value) => { const evidence = value.evidence as { cleanup: Record<string, unknown> }; evidence.cleanup.passed = false; }],
    ["failed committed project test", (value) => { const evidence = value.evidence as { projectTests: { results: Record<string, Record<string, unknown>> } }; evidence.projectTests.results.poc!.passed = false; }],
  ];
  for (const [label, mutate] of cases) {
    const candidate = structuredClone(good);
    mutate(candidate);
    assert.equal(validate(candidate), false, `${label} unexpectedly remained publication eligible`);
  }
});

test("queued and failed Builds cannot carry successful or published evidence", async () => {
  const validate = validator(await readJSON(path.join(contracts, "development-build.schema.json")));
  const good = await readJSON(path.join(fixtures, "build-good.json"));
  const queued = structuredClone(good);
  queued.status = "queued";
  assert.equal(validate(queued), false, "queued Build carried terminal evidence");
  const failed = structuredClone(good);
  failed.status = "failed";
  failed.errorCode = "build_failed";
  assert.equal(validate(failed), false, "failed Build remained publication eligible");
  const unpublished = structuredClone(good);
  (unpublished.publication as Record<string, unknown>).eligible = false;
  (unpublished.publication as Record<string, unknown>).refusalReasons = ["unauthorized"];
  (unpublished.publication as Record<string, unknown>).published = { templateVersionId: "60000000-0000-4000-8000-000000000002" };
  assert.equal(validate(unpublished), false, "ineligible Build exposed a publication receipt");
  const noReason = structuredClone(good);
  (noReason.publication as Record<string, unknown>).eligible = false;
  (noReason.publication as Record<string, unknown>).refusalReasons = [];
  assert.equal(validate(noReason), false, "ineligible Build omitted refusal reasons");
  const partialPublication = structuredClone(good);
  (partialPublication.publication as Record<string, unknown>).published = { templateVersionId: "60000000-0000-4000-8000-000000000002" };
  assert.equal(validate(partialPublication), false, "publication accepted a partial identity");
});

test("controller finalization verifies committed inputs, tenant resolution, evidence, reproducibility, and publication bindings", async () => {
  const project = await readJSON(path.join(fixtures, "project-good.json"));
  const build = await readJSON(path.join(fixtures, "build-good.json"));
  assert.equal(build.projectManifestDigest, developmentDigest(project), "good fixture project digest is stale");
  assert.equal((build.evidence as { projectTests: { definitionDigest: string } }).projectTests.definitionDigest, developmentDigest(project.tests), "good fixture test definition digest is stale");
  assert.deepEqual(verifyDevelopmentFinalization(project, build), []);
  const cases: Array<[string, (value: Record<string, unknown>) => void]> = [
    ["requester finalizer", (value) => { ((value.finalization as { authority: Record<string, unknown> }).authority).principal = "requesting-user"; }],
    ["cross-tenant Run", (value) => { ((value.finalization as { run: Record<string, unknown> }).run).workspaceId = "40000000-0000-4000-8000-000000000099"; }],
    ["cross-tenant reference Build", (value) => { ((value.finalization as { referenceBuild: Record<string, unknown> }).referenceBuild).workspaceId = "40000000-0000-4000-8000-000000000099"; }],
    ["cross-project Artifact", (value) => { ((value.finalization as { artifacts: Array<Record<string, unknown>> }).artifacts[0]!).projectId = "20000000-0000-4000-8000-000000000099"; }],
    ["wrong Artifact role kind", (value) => { ((value.finalization as { artifacts: Array<Record<string, unknown>> }).artifacts[0]!).kind = "development.test"; }],
    ["wrong Artifact media", (value) => { ((value.finalization as { artifacts: Array<Record<string, unknown>> }).artifacts[0]!).mediaType = "document"; }],
    ["missing Artifact content digest", (value) => { delete ((value.finalization as { artifacts: Array<Record<string, unknown>> }).artifacts[0]!).contentDigest; }],
    ["duplicate Artifact identity", (value) => { const artifacts = (value.finalization as { artifacts: Array<Record<string, unknown>> }).artifacts; artifacts[1]!.id = artifacts[0]!.id; }],
    ["duplicate Artifact role", (value) => { const artifacts = (value.finalization as { artifacts: Array<Record<string, unknown>> }).artifacts; artifacts[1]!.role = artifacts[0]!.role; }],
    ["unresolved Artifact", (value) => { (value.finalization as { artifacts: Array<Record<string, unknown>> }).artifacts.pop(); }],
    ["uncommitted test result", (value) => { ((value.evidence as { projectTests: { results: Record<string, unknown> } }).projectTests.results).extra = { passed: true, artifactId: "80000000-0000-4000-8000-000000000012" }; }],
    ["wrong source test evidence", (value) => { ((value.evidence as { projectTests: Record<string, unknown> }).projectTests).sourceCommit = "2".repeat(40); }],
    ["substituted source repository", (value) => { ((value.source as Record<string, unknown>)).repository = "https://github.com/attacker/substitute.git"; }],
    ["substituted builder profile", (value) => { ((value.builder as Record<string, unknown>)).profile = "untrusted-builder"; }],
    ["substituted builder identity", (value) => { ((value.builder as Record<string, unknown>)).id = "50000000-0000-4000-8000-000000000099"; }],
    ["substituted builder image", (value) => { ((value.builder as Record<string, unknown>)).imageDigest = "registry.blazn.invalid/system/other@sha256:" + "4".repeat(64); }],
    ["substituted builder version", (value) => { ((value.builder as Record<string, unknown>)).version = "v0.26.0"; }],
    ["substituted index repository", (value) => { ((value.outputs as Record<string, unknown>)).imageIndexDigest = "evil.invalid/poc/coding-agent@sha256:" + "5".repeat(64); }],
    ["substituted child repository", (value) => { (((value.outputs as { images: Array<Record<string, unknown>> }).images)[0]!).digest = "evil.invalid/poc/coding-agent@sha256:" + "6".repeat(64); }],
    ["substituted refresh repository", (value) => { (((value.outputs as { refreshArtifacts: Record<string, Record<string, unknown>> }).refreshArtifacts)["linux/amd64"]!).imageDigest = "evil.invalid/poc/coding-agent@sha256:" + "6".repeat(64); }],
    ["wrong refresh child", (value) => { (((value.outputs as { refreshArtifacts: Record<string, Record<string, unknown>> }).refreshArtifacts)["linux/arm64"]!).imageDigest = ((value.outputs as { images: Array<Record<string, unknown>> }).images[0]!).digest; }],
    ["forged refresh inputs", (value) => { (((value.outputs as { refreshArtifacts: Record<string, Record<string, unknown>> }).refreshArtifacts)["linux/arm64"]!).inputsDigest = `sha256:${"e".repeat(64)}`; }],
    ["forged refresh cache key", (value) => { (((value.outputs as { refreshArtifacts: Record<string, Record<string, unknown>> }).refreshArtifacts)["linux/amd64"]!).cacheKey = `sha256:${"e".repeat(64)}`; }],
    ["self reproducibility comparison", (value) => { const comparison = ((value.evidence as { reproducibility: { comparison: Record<string, unknown> } }).reproducibility.comparison); comparison.referenceBuildId = value.id; }],
    ["mismatched reproducibility material", (value) => { const comparison = ((value.evidence as { reproducibility: { comparison: Record<string, unknown> } }).reproducibility.comparison); comparison.referenceMaterialDigest = `sha256:${"f".repeat(64)}`; }],
    ["changed reference inputs", (value) => { ((value.finalization as { referenceBuild: { source: Record<string, unknown> } }).referenceBuild.source).commit = "2".repeat(40); }],
    ["changed reference builder tuple", (value) => { ((value.finalization as { referenceBuild: { builder: Record<string, unknown> } }).referenceBuild.builder).version = "v0.24.0"; }],
    ["forged reference receipt", (value) => { ((value.finalization as { referenceBuild: Record<string, unknown> }).referenceBuild).receiptDigest = `sha256:${"f".repeat(64)}`; }],
    ["substituted publication target", (value) => { ((value.publicationTarget as Record<string, unknown>)).templateId = "50000000-0000-4000-8000-000000000099"; }],
    ["cross-workspace publication target", (value) => { ((value.finalization as { publicationTarget: Record<string, unknown> }).publicationTarget).workspaceId = "40000000-0000-4000-8000-000000000099"; }],
  ];
  for (const [label, mutate] of cases) {
    const candidate = structuredClone(build);
    mutate(candidate);
    assert.notDeepEqual(verifyDevelopmentFinalization(project, candidate), [], `${label} unexpectedly passed finalization verification`);
  }
  const published = structuredClone(build);
  (published.publication as Record<string, unknown>).published = { templateId: "50000000-0000-4000-8000-000000000001", templateVersionId: "60000000-0000-4000-8000-000000000002", templateDigest: `sha256:${"d".repeat(64)}`, imageIndexDigest: (published.outputs as Record<string, unknown>).imageIndexDigest, buildReceiptDigest: published.receiptDigest, publishedAt: "2026-08-22T00:05:00Z" };
  assert.deepEqual(verifyDevelopmentFinalization(project, published), []);
  ((published.publication as { published: Record<string, unknown> }).published).imageIndexDigest = "registry.blazn.invalid/poc/coding-agent@sha256:" + "e".repeat(64);
  assert.notDeepEqual(verifyDevelopmentFinalization(project, published), [], "publication accepted a substituted output index");
});

test("refresh and reproducibility fixture digests are derived from exact Build inputs", async () => {
  const build = await readJSON(path.join(fixtures, "build-good.json"));
  const outputs = build.outputs as { refreshArtifacts: Record<string, { inputsDigest: string; cacheKey: string }> };
  for (const platform of ["linux/amd64", "linux/arm64"]) {
    const inputs = developmentRefreshInputsDigest(build, platform);
    assert.equal(outputs.refreshArtifacts[platform]!.inputsDigest, inputs);
    assert.equal(outputs.refreshArtifacts[platform]!.cacheKey, developmentRefreshCacheKey(inputs));
  }
  const comparison = (build.evidence as { reproducibility: { comparison: Record<string, unknown> } }).reproducibility.comparison;
  assert.equal(comparison.candidateInputDigest, developmentBuildInputDigest(build));
  assert.equal(comparison.referenceInputDigest, developmentBuildInputDigest((build.finalization as { referenceBuild: unknown }).referenceBuild));
});

test("CLI contract freezes the six acceptance commands and authority boundary", async () => {
  const rawContract = await readJSON(path.join(contracts, "development-cli-contract.json"));
  const contract = rawContract as unknown as {
    commands: Record<string, { mutation: boolean; authentication: boolean; arguments: { required?: string[] }; outputSchema: { $ref: string } }>;
    securityBoundary: { clientMayNotAssert: string[]; neverReturned: string[] };
  };
  assert.deepEqual(Object.keys(contract.commands), ["dev validate", "dev build", "dev test", "dev status", "dev evidence", "dev publish"]);
  assert.equal(contract.commands["dev validate"]!.mutation, false);
  assert.equal(contract.commands["dev validate"]!.authentication, false);
  assert.deepEqual(contract.commands["dev build"]!.arguments.required, ["--ref", "--request-id"]);
  assert.deepEqual(contract.commands["dev publish"]!.arguments.required, ["BUILD", "--expected-version", "--request-id"]);
  for (const field of ["builderIdentity", "outputDigest", "testResult", "secretScanResult", "provenance", "publicationEligibility"]) assert.ok(contract.securityBoundary.clientMayNotAssert.includes(field));
  for (const field of ["buildkitEndpoint", "buildkitClientCertificate", "registryCredential", "objectKey", "signedUrl", "secretValue"]) assert.ok(contract.securityBoundary.neverReturned.includes(field));

  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  ajv.addSchema(await readJSON(path.join(contracts, "development-build.schema.json")));
  ajv.addSchema(rawContract, "development-cli");
  const build = await readJSON(path.join(fixtures, "build-good.json"));
  const outputs: Record<string, unknown> = {
    "dev validate": { valid: true, manifestDigest: `sha256:${"a".repeat(64)}`, errors: [], warnings: [] },
    "dev build": build,
    "dev test": build,
    "dev status": build,
    "dev evidence": { buildId: build.id, directory: "/tmp/evidence", manifestDigest: `sha256:${"b".repeat(64)}`, artifactIds: ["80000000-0000-4000-8000-000000000001"] },
    "dev publish": build,
  };
  for (const [command, output] of Object.entries(outputs)) {
    const validate = ajv.getSchema(`development-cli${contract.commands[command]!.outputSchema.$ref}`);
    assert.ok(validate, `unresolved output schema for ${command}`);
    assert.equal(validate(output), true, `${command}: ${JSON.stringify(validate.errors)}`);
  }
});

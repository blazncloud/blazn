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
});

test("grant file header schema rejects isolated dot segments", async () => {
  const openapi = await readJSON(path.join(contracts, "sandboxes.openapi.json")) as { components: { parameters: { SandboxPath: { schema: Record<string, unknown> } } } };
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  const validate = ajv.compile(openapi.components.parameters.SandboxPath.schema);
  assert.equal(validate("/workspace/src/repo/file"), true);
  assert.equal(validate("/workspace/src/./escape"), false);
  assert.equal(validate("/workspace/src/../escape"), false);
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
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  ajv.addSchema(template, "sandbox-template.schema.json");
  ajv.addSchema(contract, "sandbox-cli");
  const exec = ajv.getSchema("sandbox-cli#/$defs/execResult");
  const error = ajv.getSchema("sandbox-cli#/errorEnvelope");
  assert.ok(exec && error);
  assert.equal(exec(await readJSON(path.join(fixtures, "cli-exec-success.json"))), true, JSON.stringify(exec.errors));
  assert.equal(error(await readJSON(path.join(fixtures, "cli-error.json"))), true, JSON.stringify(error.errors));
});

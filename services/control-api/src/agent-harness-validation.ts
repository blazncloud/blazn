import { createRequire } from "node:module";
import { Ajv2020, type ValidateFunction } from "ajv/dist/2020.js";
import type { FormatsPlugin } from "ajv-formats";
import { agentBundleSchema, harnessBundleSchema, harnessDefinitionSchema, harnessProfileSchema, harnessVersionSchema } from "./agent-harness-schemas.generated.js";

const require = createRequire(import.meta.url);
const formatsModule = require("ajv-formats") as { default?: FormatsPlugin } | FormatsPlugin;
const addFormats = ("default" in formatsModule ? formatsModule.default : formatsModule) as FormatsPlugin;

const ajv = new Ajv2020({ allErrors: true, strict: false });
addFormats(ajv);

const compile = (schema: unknown) => ajv.compile(schema as Record<string, unknown>);
const agentBundle = compile(agentBundleSchema);
const harnessBundle = compile(harnessBundleSchema);
const harnessDefinition = compile(harnessDefinitionSchema);
const harnessVersion = compile(harnessVersionSchema);
const harnessProfile = compile(harnessProfileSchema);

function errors(fn: ValidateFunction, value: unknown): string[] {
  if (fn(value)) return [];
  return (fn.errors ?? []).slice(0, 32).map((error) => `${error.instancePath || "$"} ${error.message ?? "is invalid"}`);
}

export const validateAgentBundleSchema = (value: unknown) => errors(agentBundle, value);
export const validateHarnessBundleSchema = (value: unknown) => errors(harnessBundle, value);
export const validateHarnessDefinitionSchema = (value: unknown) => errors(harnessDefinition, value);
export const validateHarnessVersionSchema = (value: unknown) => errors(harnessVersion, value);
export const validateHarnessProfileSchema = (value: unknown) => errors(harnessProfile, value);

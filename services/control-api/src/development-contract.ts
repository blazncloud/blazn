import { createHash } from "node:crypto";

type ObjectValue = Record<string, unknown>;

function object(value: unknown): ObjectValue | undefined {
  return typeof value === "object" && value !== null && !Array.isArray(value) ? value as ObjectValue : undefined;
}
function text(value: unknown): string { return typeof value === "string" ? value : ""; }
function canonical(value: unknown): string {
  if (value === null || typeof value !== "object") return JSON.stringify(value);
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
  const record = value as ObjectValue;
  return `{${Object.keys(record).sort().map((key) => `${JSON.stringify(key)}:${canonical(record[key])}`).join(",")}}`;
}
export function developmentDigest(value: unknown): string {
  return `sha256:${createHash("sha256").update(canonical(value)).digest("hex")}`;
}

const shellNames = new Set(["sh", "bash", "dash", "zsh", "fish", "ksh", "csh", "tcsh", "env", "cmd", "cmd.exe", "powershell", "powershell.exe", "pwsh"]);
const secretFlag = /^--?[a-z0-9_-]*(?:api[_-]?key|token|secret|password|credential|authorization)[a-z0-9_-]*(?:=|$)/i;
const secretAssignment = /^(?:[A-Z0-9_]*(?:API_KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL|AUTHORIZATION)[A-Z0-9_]*)=/i;

export function verifyDevelopmentProjectCommands(project: unknown): string[] {
  const errors: string[] = [];
  const tests = object(object(project)?.tests);
  if (!tests) return ["project tests are missing"];
  for (const [name, raw] of Object.entries(tests)) {
    const argv = object(raw)?.argv;
    if (!Array.isArray(argv) || argv.some((item) => typeof item !== "string")) {
      errors.push(`test ${name} argv is invalid`);
      continue;
    }
    const executable = (argv[0] as string).split("/").at(-1)?.toLowerCase() ?? "";
    if (shellNames.has(executable)) errors.push(`test ${name} directly invokes a shell or env launcher`);
    for (const argument of argv as string[]) {
      if (secretFlag.test(argument) || secretAssignment.test(argument) || /bearer\s+\S/i.test(argument) || /:\/\/[^/\s]+@/.test(argument)) {
        errors.push(`test ${name} argv contains credential-like material`);
        break;
      }
    }
  }
  return errors;
}

function sameJSON(left: unknown, right: unknown): boolean { return canonical(left) === canonical(right); }
function collectTypedArtifactIDs(outputs: ObjectValue, evidence: ObjectValue): Set<string> {
  const ids = new Set<string>();
  const add = (value: unknown) => { if (typeof value === "string") ids.add(value); };
  for (const refresh of Object.values(object(outputs.refreshArtifacts) ?? {})) add(object(refresh)?.artifactId);
  add(evidence.provenanceArtifactId); add(evidence.signatureArtifactId); add(evidence.sbomArtifactId);
  for (const result of Object.values(object(object(evidence.projectTests)?.results) ?? {})) add(object(result)?.artifactId);
  for (const name of ["securityTests", "lifecycleTests"] as const) for (const result of Array.isArray(evidence[name]) ? evidence[name] as unknown[] : []) add(object(result)?.artifactId);
  add(object(evidence.cleanup)?.artifactId);
  add(object(object(evidence.reproducibility)?.comparison)?.artifactId);
  add(object(evidence.reproducibility)?.reviewArtifactId);
  return ids;
}

// This verifier is the machine-readable cross-resource half of the JSON
// schemas. A controller must run it after resolving the canonical Project, Run,
// and Artifact rows under its mTLS workload identity and before finalization.
export function verifyDevelopmentFinalization(projectValue: unknown, buildValue: unknown): string[] {
  const errors = verifyDevelopmentProjectCommands(projectValue);
  const project = object(projectValue), build = object(buildValue);
  if (!project || !build) return [...errors, "project or build is not an object"];
  const outputs = object(build.outputs), evidence = object(build.evidence), finalization = object(build.finalization);
  if (!outputs || !evidence || !finalization) return [...errors, "terminal build outputs, evidence, and finalization are required"];

  if (text(project.projectId) !== text(build.projectId)) errors.push("DevelopmentProject does not match Build projectId");
  if (developmentDigest(project) !== text(build.projectManifestDigest)) errors.push("projectManifestDigest does not bind the resolved committed project");
  if (!sameJSON(project.template, build.template) || !sameJSON(project.platforms, build.platforms) || !sameJSON(project.dependencyLocks, build.dependencyLocks)) errors.push("Build inputs do not match the resolved DevelopmentProject");

  const projectTests = object(project.tests) ?? {};
  const testEvidence = object(evidence.projectTests) ?? {};
  if (text(testEvidence.sourceCommit) !== text(object(build.source)?.commit)) errors.push("project test evidence does not bind the Build source commit");
  if (text(testEvidence.definitionDigest) !== developmentDigest(projectTests)) errors.push("project test evidence does not bind committed test definitions");
  const resultNames = Object.keys(object(testEvidence.results) ?? {}).sort();
  if (!sameJSON(resultNames, Object.keys(projectTests).sort())) errors.push("project test evidence is incomplete or contains uncommitted tests");

  const authority = object(finalization.authority) ?? {};
  if (authority.contractVersion !== "blazn.dev/finalizer/v1alpha1" || authority.kind !== "controller" || authority.principal !== "blazn-development-controller" || authority.authentication !== "mtls-workload-identity") errors.push("Build was not finalized by the frozen controller authority");
  const run = object(finalization.run) ?? {};
  if (run.id !== build.runId || run.workspaceId !== build.workspaceId || run.projectId !== build.projectId) errors.push("resolved Run is outside the Build tenant or identity");

  const declared = new Set((Array.isArray(evidence.artifactIds) ? evidence.artifactIds : []).filter((id): id is string => typeof id === "string"));
  const typed = collectTypedArtifactIDs(outputs, evidence);
  for (const id of typed) if (!declared.has(id)) errors.push(`typed evidence Artifact ${id} is absent from artifactIds`);
  const resolved = new Set<string>();
  for (const raw of Array.isArray(finalization.artifacts) ? finalization.artifacts : []) {
    const artifact = object(raw) ?? {};
    const id = text(artifact.id);
    if (!id || resolved.has(id)) errors.push("resolved Artifact identities must be unique");
    resolved.add(id);
    if (artifact.workspaceId !== build.workspaceId || artifact.projectId !== build.projectId) errors.push(`resolved Artifact ${id} is outside the Build tenant`);
  }
  if (!sameJSON([...declared].sort(), [...resolved].sort())) errors.push("resolved Artifacts do not exactly match Build artifactIds");

  const imageByPlatform = new Map<string, string>();
  for (const raw of Array.isArray(outputs.images) ? outputs.images : []) { const image = object(raw) ?? {}; imageByPlatform.set(text(image.platform), text(image.digest)); }
  for (const [platform, raw] of Object.entries(object(outputs.refreshArtifacts) ?? {})) if (text(object(raw)?.imageDigest) !== imageByPlatform.get(platform)) errors.push(`refresh Artifact ${platform} is not bound to its exact image child`);

  const reproduction = object(evidence.reproducibility) ?? {}, comparison = object(reproduction.comparison) ?? {};
  const referenceBuild = object(finalization.referenceBuild) ?? {};
  if (referenceBuild.id !== comparison.referenceBuildId || referenceBuild.workspaceId !== build.workspaceId || referenceBuild.projectId !== build.projectId) errors.push("reproducibility reference Build is unresolved or outside the Build tenant");
  if (comparison.candidateBuildId !== build.id || comparison.candidateMaterialDigest !== outputs.materialDigest) errors.push("reproducibility comparison does not bind this Build and material");
  if (comparison.referenceBuildId === build.id) errors.push("reproducibility comparison must use a distinct reference Build");
  if (reproduction.outcome === "same-material-digest" && comparison.referenceMaterialDigest !== comparison.candidateMaterialDigest) errors.push("same-material reproducibility has unequal material digests");
  if (reproduction.outcome === "explained-nondeterminism" && comparison.referenceMaterialDigest === comparison.candidateMaterialDigest) errors.push("explained nondeterminism has no material difference");

  const published = object(object(build.publication)?.published);
  if (published && (published.imageIndexDigest !== outputs.imageIndexDigest || published.buildReceiptDigest !== build.receiptDigest)) errors.push("publication identity is not bound to the exact output index and Build receipt");
  return errors;
}

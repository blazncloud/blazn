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
const secretHeader = /(?:^|[=\s]|-h)(?:authorization|proxy-authorization|x-api-key|api-key)\s*[:=]/i;
const secretQuery = /[?&](?:api[_-]?key|access[_-]?token|auth[_-]?token|token|secret|password|credential|authorization)=/i;

function decodedArgument(value: string): string {
  try { return decodeURIComponent(value); } catch { return value; }
}

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
      const decoded = decodedArgument(argument);
      if (secretFlag.test(decoded) || secretAssignment.test(decoded) || secretHeader.test(decoded) || secretQuery.test(decoded) || /bearer\s+\S/i.test(decoded) || /:\/\/[^/\s]+@/.test(decoded)) {
        errors.push(`test ${name} argv contains credential-like material`);
        break;
      }
    }
  }
  return errors;
}

function sameJSON(left: unknown, right: unknown): boolean { return canonical(left) === canonical(right); }
function buildInputIdentity(value: ObjectValue): ObjectValue {
  return {
    schemaVersion: "blazn.dev/build-input/v1alpha1",
    source: value.source,
    projectManifestDigest: value.projectManifestDigest,
    buildContextDigest: value.buildContextDigest,
    template: value.template,
    dependencyLocks: value.dependencyLocks,
    planDigest: value.planDigest,
    platforms: value.platforms,
    builderProfile: object(value.builder)?.profile ?? value.builderProfile,
  };
}
export function developmentBuildInputDigest(value: unknown): string {
  return developmentDigest(buildInputIdentity(object(value) ?? {}));
}
export function developmentRefreshInputsDigest(buildValue: unknown, platform: string): string {
  const build = object(buildValue) ?? {};
  return developmentDigest({schemaVersion: "blazn.dev/refresh-input/v1alpha1", platform, template: build.template, source: build.source, dependencyLocks: build.dependencyLocks, buildContextDigest: build.buildContextDigest, planDigest: build.planDigest});
}
export function developmentRefreshCacheKey(inputsDigest: string): string {
  return developmentDigest({schemaVersion: "blazn.dev/refresh-cache/v1alpha1", inputsDigest});
}
function ociRepository(value: unknown): string { return text(value).split("@", 1)[0] ?? ""; }
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
  if (text(object(project.repository)?.url) !== text(object(build.source)?.repository)) errors.push("Build source repository does not match the DevelopmentProject repository");
  if (text(object(project.policy)?.builderProfile) !== text(object(build.builder)?.profile)) errors.push("Build builder profile does not match DevelopmentProject policy");
  const registryRepository = text(object(project.build)?.registryRepository);
  if (ociRepository(outputs.imageIndexDigest) !== registryRepository) errors.push("output index is outside the DevelopmentProject registry repository");

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
  for (const raw of Array.isArray(outputs.images) ? outputs.images : []) {
    const image = object(raw) ?? {}, digest = text(image.digest);
    imageByPlatform.set(text(image.platform), digest);
    if (ociRepository(digest) !== registryRepository) errors.push(`output image ${text(image.platform)} is outside the DevelopmentProject registry repository`);
  }
  for (const [platform, raw] of Object.entries(object(outputs.refreshArtifacts) ?? {})) {
    const refresh = object(raw) ?? {};
    if (text(refresh.imageDigest) !== imageByPlatform.get(platform)) errors.push(`refresh Artifact ${platform} is not bound to its exact image child`);
    if (ociRepository(refresh.imageDigest) !== registryRepository) errors.push(`refresh Artifact ${platform} is outside the DevelopmentProject registry repository`);
    const inputsDigest = developmentRefreshInputsDigest(build, platform);
    if (refresh.inputsDigest !== inputsDigest || refresh.cacheKey !== developmentRefreshCacheKey(inputsDigest)) errors.push(`refresh Artifact ${platform} does not derive its input digest and cache key from the frozen Build inputs`);
  }

  const reproduction = object(evidence.reproducibility) ?? {}, comparison = object(reproduction.comparison) ?? {};
  const referenceBuild = object(finalization.referenceBuild) ?? {};
  const candidateInputDigest = developmentBuildInputDigest(build), referenceInputDigest = developmentBuildInputDigest(referenceBuild);
  if (referenceBuild.id !== comparison.referenceBuildId || referenceBuild.workspaceId !== build.workspaceId || referenceBuild.projectId !== build.projectId || referenceBuild.receiptDigest !== comparison.referenceReceiptDigest) errors.push("reproducibility reference Build is unresolved or outside the Build tenant");
  if (referenceBuild.inputDigest !== referenceInputDigest || comparison.referenceInputDigest !== referenceInputDigest || comparison.referenceMaterialDigest !== referenceBuild.materialDigest) errors.push("reproducibility reference Build digest or material is not bound to its resolved inputs");
  if (comparison.candidateBuildId !== build.id || comparison.candidateInputDigest !== candidateInputDigest || comparison.candidateMaterialDigest !== outputs.materialDigest) errors.push("reproducibility comparison does not bind this Build, inputs, and material");
  if (referenceInputDigest !== candidateInputDigest) errors.push("reproducibility comparison did not use unchanged material inputs");
  if (comparison.referenceBuildId === build.id) errors.push("reproducibility comparison must use a distinct reference Build");
  if (reproduction.outcome === "same-material-digest" && comparison.referenceMaterialDigest !== comparison.candidateMaterialDigest) errors.push("same-material reproducibility has unequal material digests");
  if (reproduction.outcome === "explained-nondeterminism" && comparison.referenceMaterialDigest === comparison.candidateMaterialDigest) errors.push("explained nondeterminism has no material difference");

  const published = object(object(build.publication)?.published);
  const target = object(build.publicationTarget) ?? {}, declaredTarget = object(project.publicationTarget) ?? {}, resolvedTarget = object(finalization.publicationTarget) ?? {};
  if (target.templateId !== declaredTarget.templateId || resolvedTarget.workspaceId !== build.workspaceId || !sameJSON(target, {templateId: resolvedTarget.templateId, candidateVersionId: resolvedTarget.candidateVersionId, candidateDigest: resolvedTarget.candidateDigest, expectedDraftVersion: resolvedTarget.expectedDraftVersion})) errors.push("publication target is not authorized by the committed project and resolved Workspace template");
  if (published && (published.templateId !== target.templateId || published.templateVersionId !== target.candidateVersionId || published.templateDigest !== target.candidateDigest || published.imageIndexDigest !== outputs.imageIndexDigest || published.buildReceiptDigest !== build.receiptDigest)) errors.push("publication identity is not bound to the authorized target, exact output index, and Build receipt");
  return errors;
}

import { createHash } from "node:crypto";
import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

const artifactPath = "/workspace/artifacts/change.patch";
const maxArtifactBytes = 8 * 1024 * 1024;

export function digest(value) {
  return `sha256:${createHash("sha256").update(value).digest("hex")}`;
}

export function solveTask(task, source) {
  if (task.schemaVersion !== "blazn.dev/coding-task/v1alpha1") throw new Error("unsupported coding task contract");
  if (digest(source) !== task.sourceDigest) throw new Error("source digest does not match the immutable task");
  const first = source.indexOf(task.find);
  if (first < 0 || source.indexOf(task.find, first + task.find.length) >= 0) {
    throw new Error("task replacement must match exactly once");
  }
  return `${source.slice(0, first)}${task.replace}${source.slice(first + task.find.length)}`;
}

export function createPatch(task, source) {
  if (typeof task.sourcePath !== "string" || task.sourcePath.length > 4096 || path.posix.isAbsolute(task.sourcePath) ||
      task.sourcePath.split("/").some((part) => !part || part === "." || part === ".." || !/^[A-Za-z0-9._-]+$/.test(part))) {
    throw new Error("task source path is invalid");
  }
  const modified = solveTask(task, source);
  const before = source.trimEnd().split("\n"), after = modified.trimEnd().split("\n");
  const lines = [`--- a/${task.sourcePath}`, `+++ b/${task.sourcePath}`, `@@ -1,${before.length} +1,${after.length} @@`];
  for (let index = 0; index < before.length; index++) {
    if (before[index] === after[index]) lines.push(` ${before[index]}`);
    else lines.push(`-${before[index]}`, `+${after[index]}`);
  }
  const patch = `${lines.join("\n")}\n`;
  if (Buffer.byteLength(patch) > maxArtifactBytes) throw new Error("patch artifact exceeds the Phase 5 bound");
  return patch;
}

export async function writePatchArtifact(taskPath, sourceRoot, outputPath) {
  const task = JSON.parse(await readFile(taskPath, "utf8"));
  const source = await readFile(path.join(sourceRoot, task.sourcePath), "utf8");
  const patch = createPatch(task, source);
  await writeFile(outputPath, patch, { flag: "wx", mode: 0o600 });
  return patch;
}

async function main(argv) {
  if (argv.length !== 6 || argv[0] !== "--task" || argv[2] !== "--source-root" || argv[4] !== "--output" || argv[5] !== artifactPath) {
    throw new Error("usage: coding-agent --task TASK_JSON --source-root SOURCE_ROOT --output /workspace/artifacts/change.patch");
  }
  await writePatchArtifact(argv[1], argv[3], argv[5]);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).catch((error) => { process.stderr.write(`${error.message}\n`); process.exitCode = 1; });
}

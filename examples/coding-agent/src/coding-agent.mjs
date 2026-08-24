import { createHash } from "node:crypto";
import { lstat, readFile, realpath, writeFile } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

const artifactPath = "/workspace/artifacts/change.patch";
const maxArtifactBytes = 8 * 1024 * 1024;
const maxTaskBytes = 64 * 1024;
const maxSourceBytes = 4 * 1024 * 1024;

export function digest(value) {
  return `sha256:${createHash("sha256").update(value).digest("hex")}`;
}

export function solveTask(task, source) {
  validateTask(task);
  if (digest(source) !== task.sourceDigest) throw new Error("source digest does not match the immutable task");
  const first = source.indexOf(task.find);
  if (first < 0 || source.indexOf(task.find, first + task.find.length) >= 0) {
    throw new Error("task replacement must match exactly once");
  }
  return `${source.slice(0, first)}${task.replace}${source.slice(first + task.find.length)}`;
}

export function createPatch(task, source) {
  validateTask(task);
  const modified = solveTask(task, source);
  const before = splitLines(source), after = splitLines(modified);
  if (before.length !== after.length) throw new Error("task replacement must preserve line topology");
  const lines = [`--- a/${task.sourcePath}`, `+++ b/${task.sourcePath}`, `@@ -1,${before.length} +1,${after.length} @@`];
  for (let index = 0; index < before.length; index++) {
    if (before[index].content === after[index].content && before[index].newline === after[index].newline) {
      appendPatchLine(lines, " ", before[index]);
    } else {
      appendPatchLine(lines, "-", before[index]);
      appendPatchLine(lines, "+", after[index]);
    }
  }
  const patch = `${lines.join("\n")}\n`;
  if (Buffer.byteLength(patch) > maxArtifactBytes) throw new Error("patch artifact exceeds the Phase 5 bound");
  return patch;
}

function splitLines(value) {
  const lines = [];
  let start = 0;
  while (start < value.length) {
    const end = value.indexOf("\n", start);
    if (end < 0) {
      lines.push({ content: value.slice(start), newline: false });
      break;
    }
    lines.push({ content: value.slice(start, end), newline: true });
    start = end + 1;
  }
  return lines;
}

function appendPatchLine(patch, prefix, line) {
  patch.push(`${prefix}${line.content}`);
  if (!line.newline) patch.push("\\ No newline at end of file");
}

export async function writePatchArtifact(taskPath, sourceRoot, outputPath) {
  const task = JSON.parse((await readStableFile(taskPath, maxTaskBytes, "task")).toString("utf8"));
  validateTask(task);
  const rootInfo=await lstat(sourceRoot);
  if(!rootInfo.isDirectory()||rootInfo.isSymbolicLink())throw new Error("source root is invalid");
  const canonicalRoot=await realpath(sourceRoot),parts=task.sourcePath.split("/");
  let cursor=canonicalRoot;
  for(const part of parts.slice(0,-1)){cursor=path.join(cursor,part);const info=await lstat(cursor);if(!info.isDirectory()||info.isSymbolicLink())throw new Error("source path component is invalid");}
  const candidate=path.resolve(canonicalRoot,...parts);
  if(!candidate.startsWith(`${canonicalRoot}${path.sep}`))throw new Error("source path escapes its root");
  const source=(await readStableFile(candidate,maxSourceBytes,"source")).toString("utf8");
  const patch = createPatch(task, source);
  await writeFile(outputPath, patch, { flag: "wx", mode: 0o600 });
  return patch;
}

function validateTask(task){
  if(!task||typeof task!=="object"||task.schemaVersion!=="blazn.dev/coding-task/v1alpha1")throw new Error("unsupported coding task contract");
  if(typeof task.sourcePath!=="string"||task.sourcePath.length>4096||path.posix.isAbsolute(task.sourcePath)||task.sourcePath.split("/").some((part)=>!part||part==="."||part===".."||!/^[A-Za-z0-9._-]+$/.test(part)))throw new Error("task source path is invalid");
  for(const value of [task.find,task.replace])if(typeof value!=="string"||!value||Buffer.byteLength(value)>64*1024||value.includes("\0"))throw new Error("task replacement is invalid");
  if(typeof task.sourceDigest!=="string"||!/^sha256:[0-9a-f]{64}$/.test(task.sourceDigest))throw new Error("task source digest is invalid");
}

async function readStableFile(file,maxBytes,label){
  const before=await lstat(file);if(!before.isFile()||before.isSymbolicLink()||before.nlink!==1||before.size>maxBytes)throw new Error(`${label} file is unsafe`);
  const content=await readFile(file),after=await lstat(file);
  if(!after.isFile()||after.isSymbolicLink()||after.nlink!==1||after.dev!==before.dev||after.ino!==before.ino||after.size!==before.size||after.mtimeMs!==before.mtimeMs||after.ctimeMs!==before.ctimeMs)throw new Error(`${label} file changed while reading`);
  return content;
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

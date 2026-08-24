import assert from "node:assert/strict";
import { mkdir, mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { createPatch, digest, solveTask, writePatchArtifact } from "../src/coding-agent.mjs";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const repo = path.resolve(root, "../..");

test("the immutable minimal coding task produces one deterministic edit", async () => {
  const task = JSON.parse(await readFile(path.join(root, "fixtures/task.json"), "utf8"));
  const source = await readFile(path.join(repo, task.sourcePath), "utf8");
  assert.equal(digest(source), task.sourceDigest);
  const first = solveTask(task, source);
  const second = solveTask(task, source);
  assert.equal(first, second);
  assert.equal(first, "export function add(left, right) {\n  return left + right;\n}\n");
});

test("the runtime writes the exact bounded Phase 5 patch artifact", async () => {
  const task = JSON.parse(await readFile(path.join(root, "fixtures/task.json"), "utf8"));
  const source = await readFile(path.join(repo, task.sourcePath), "utf8");
  const expected = `--- a/${task.sourcePath}\n+++ b/${task.sourcePath}\n@@ -1,3 +1,3 @@\n export function add(left, right) {\n-  return left - right;\n+  return left + right;\n }\n`;
  assert.equal(createPatch(task, source), expected);
  assert.ok(Buffer.byteLength(expected) <= 8 * 1024 * 1024);
  const temporary = await mkdtemp(path.join(os.tmpdir(), "blazn-coding-agent-"));
  try {
    const sourceRoot = path.join(temporary, "source"), output = path.join(temporary, "artifacts/change.patch");
    await mkdir(path.dirname(path.join(sourceRoot, task.sourcePath)), { recursive: true });
    await mkdir(path.dirname(output), { recursive: true });
    await writeFile(path.join(sourceRoot, task.sourcePath), source);
    assert.equal(await writePatchArtifact(path.join(root, "fixtures/task.json"), sourceRoot, output), expected);
    assert.equal(await readFile(output, "utf8"), expected);
    assert.equal((await stat(output)).mode & 0o777, 0o600);
    await assert.rejects(writePatchArtifact(path.join(root, "fixtures/task.json"), sourceRoot, output), /EEXIST/);
  } finally { await rm(temporary, { recursive: true, force: true }); }
});

test("changed source cannot be substituted into the task", async () => {
  const task = JSON.parse(await readFile(path.join(root, "fixtures/task.json"), "utf8"));
  assert.throws(() => solveTask(task, "export const substituted = true;\n"), /source digest/);
});

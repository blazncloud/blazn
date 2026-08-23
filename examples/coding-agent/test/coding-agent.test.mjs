import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { digest, solveTask } from "../src/coding-agent.mjs";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

test("the immutable minimal coding task produces one deterministic edit", async () => {
  const task = JSON.parse(await readFile(path.join(root, "fixtures/task.json"), "utf8"));
  const source = await readFile(path.join(root, task.sourcePath), "utf8");
  assert.equal(digest(source), task.sourceDigest);
  const first = solveTask(task, source);
  const second = solveTask(task, source);
  assert.equal(first, second);
  assert.equal(first, "export function add(left, right) {\n  return left + right;\n}\n");
});

test("changed source cannot be substituted into the task", async () => {
  const task = JSON.parse(await readFile(path.join(root, "fixtures/task.json"), "utf8"));
  assert.throws(() => solveTask(task, "export const substituted = true;\n"), /source digest/);
});

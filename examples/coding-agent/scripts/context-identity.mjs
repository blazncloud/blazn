import { createHash } from "node:crypto";
import { lstat, readFile, readdir } from "node:fs/promises";
import path from "node:path";

export const contextFiles = [
  ".dockerignore",
  "Dockerfile",
  "fixtures/source/calculator.mjs",
  "fixtures/task.json",
  "package-lock.json",
  "package.json",
  "scripts/context-identity.mjs",
  "src/coding-agent.mjs",
  "test/coding-agent.test.mjs",
  "test/context-identity.test.mjs"
];
const closedDirectories = {
  "fixtures/source": ["calculator.mjs"],
  src: ["coding-agent.mjs"]
};
const sha256 = (value) => `sha256:${createHash("sha256").update(value).digest("hex")}`;
const canonical = (value) => {
  if (value === null || typeof value !== "object") return JSON.stringify(value);
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
  return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${canonical(value[key])}`).join(",")}}`;
};

export async function buildContextIdentity(root) {
  for (const [directory, expected] of Object.entries(closedDirectories)) {
    const info = await lstat(path.join(root, directory));
    if (!info.isDirectory() || info.isSymbolicLink() || (info.mode & 0o777) !== 0o755) throw new Error(`unsafe context directory ${directory}`);
    const actual = (await readdir(path.join(root, directory))).sort();
    if (canonical(actual) !== canonical([...expected].sort())) throw new Error(`unexpected file in closed directory ${directory}`);
  }
  const entries = {};
  for (const name of contextFiles) {
    const info = await lstat(path.join(root, name));
    if (!info.isFile() || info.isSymbolicLink() || info.nlink !== 1 || (info.mode & 0o777) !== 0o644) throw new Error(`unsafe context file ${name}`);
    entries[name] = { mode: "0644", digest: sha256(await readFile(path.join(root, name))) };
  }
  return sha256(`blazn-example-build-context-v2\n${canonical(entries)}`);
}

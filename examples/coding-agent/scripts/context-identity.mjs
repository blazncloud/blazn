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
const contextDirectories = ["fixtures", "fixtures/source", "scripts", "src", "test"];
const sha256 = (value) => `sha256:${createHash("sha256").update(value).digest("hex")}`;
const canonical = (value) => {
  if (value === null || typeof value !== "object") return JSON.stringify(value);
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
  return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${canonical(value[key])}`).join(",")}}`;
};

export async function buildContextIdentity(root) {
  const directories = new Map();
  for (const directory of contextDirectories) {
    const info = await lstat(path.join(root, directory));
    if (!info.isDirectory() || info.isSymbolicLink() || (info.mode & 0o777) !== 0o755) throw new Error(`unsafe context directory ${directory}`);
    directories.set(directory, { dev: info.dev, ino: info.ino });
  }
  for (const [directory, expected] of Object.entries(closedDirectories)) {
    const actual = (await readdir(path.join(root, directory))).sort();
    if (canonical(actual) !== canonical([...expected].sort())) throw new Error(`unexpected file in closed directory ${directory}`);
  }
  const entries = {};
  for (const name of contextFiles) {
    const target = path.join(root, name), before = await lstat(target);
    if (!before.isFile() || before.isSymbolicLink() || before.nlink !== 1 || (before.mode & 0o777) !== 0o644) throw new Error(`unsafe context file ${name}`);
    const content = await readFile(target), after = await lstat(target);
    if (!after.isFile() || after.isSymbolicLink() || after.nlink !== 1 || after.dev !== before.dev || after.ino !== before.ino || after.size !== before.size || after.mtimeMs !== before.mtimeMs || after.ctimeMs !== before.ctimeMs) throw new Error(`context file changed while reading ${name}`);
    entries[name] = { mode: "0644", digest: sha256(content) };
  }
  for (const [directory, identity] of directories) {
    const after = await lstat(path.join(root, directory));
    if (!after.isDirectory() || after.isSymbolicLink() || after.dev !== identity.dev || after.ino !== identity.ino) throw new Error(`context directory changed while reading ${directory}`);
  }
  return sha256(`blazn-example-build-context-v2\n${canonical(entries)}`);
}

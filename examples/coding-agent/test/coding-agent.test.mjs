import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { mkdir, mkdtemp, readFile, rm, stat, symlink, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { createPatch, digest, solveTask, writePatchArtifact } from "../src/coding-agent.mjs";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const repo = path.resolve(root, "../..");
const execFileAsync = promisify(execFile);

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

test("patch artifacts preserve exact EOF bytes and pass git apply --check", async () => {
  const cases = [
    { name: "no-final-newline", source: "value = old", expected: "value = new" },
    { name: "trailing-whitespace", source: "value = old  \n", expected: "value = new  \n" },
    { name: "trailing-blank-lines", source: "value = old\n\n\n", expected: "value = new\n\n\n" },
  ];
  for (const fixture of cases) {
    const temporary = await mkdtemp(path.join(os.tmpdir(), `blazn-coding-agent-${fixture.name}-`));
    try {
      const sourcePath = "fixtures/exact.txt", patchPath = path.join(temporary, "change.patch");
      const task = { schemaVersion: "blazn.dev/coding-task/v1alpha1", sourcePath, sourceDigest: digest(fixture.source), find: "old", replace: "new" };
      const patch = createPatch(task, fixture.source);
      await mkdir(path.join(temporary, "fixtures"));
      await writeFile(path.join(temporary, sourcePath), fixture.source);
      await writeFile(patchPath, patch);
      await execFileAsync("git", ["apply", "--check", patchPath], { cwd: temporary });
      await execFileAsync("git", ["apply", patchPath], { cwd: temporary });
      assert.equal(await readFile(path.join(temporary, sourcePath), "utf8"), fixture.expected);
      if (fixture.name === "no-final-newline") assert.match(patch, /\\ No newline at end of file/);
    } finally { await rm(temporary, { recursive: true, force: true }); }
  }
});

test("changed source cannot be substituted into the task", async () => {
  const task = JSON.parse(await readFile(path.join(root, "fixtures/task.json"), "utf8"));
  assert.throws(() => solveTask(task, "export const substituted = true;\n"), /source digest/);
});

test("runtime source I/O rejects traversal and symlink substitution before output",async()=>{
  const temporary=await mkdtemp(path.join(os.tmpdir(),"blazn-coding-agent-unsafe-"));
  try{
    const root=path.join(temporary,"root"),outside=path.join(temporary,"outside.mjs"),taskPath=path.join(temporary,"task.json"),output=path.join(temporary,"change.patch");
    await mkdir(root);await writeFile(outside,"export default true;\n");
    const base={schemaVersion:"blazn.dev/coding-task/v1alpha1",id:"unsafe",sourceDigest:digest("export default true;\n"),find:"true",replace:"false"};
    await writeFile(taskPath,JSON.stringify({...base,sourcePath:"../outside.mjs"}));await assert.rejects(writePatchArtifact(taskPath,root,output),/source path is invalid/);
    await writeFile(taskPath,JSON.stringify({...base,sourcePath:"linked.mjs"}));await symlink(outside,path.join(root,"linked.mjs"));await assert.rejects(writePatchArtifact(taskPath,root,output),/source file is unsafe/);
    await rm(taskPath);await symlink(outside,taskPath);await assert.rejects(writePatchArtifact(taskPath,root,output),/task file is unsafe/);
  }finally{await rm(temporary,{recursive:true,force:true});}
});

test("Docker verify and runtime stages preserve the mounted-source layout",async()=>{
  const dockerfile=await readFile(path.join(root,"Dockerfile"),"utf8");
  assert.match(dockerfile,/WORKDIR \/workspace\/src\/blazn\/examples\/coding-agent/);
  assert.match(dockerfile,/COPY --from=verify --chown=1000:1000 \/workspace\/src\/blazn\/examples\/coding-agent \/opt\/coding-agent/);
  assert.match(dockerfile,/WORKDIR \/opt\/coding-agent/);
  assert.match(dockerfile,/CMD \["--task", "\/workspace\/src\/blazn\/examples\/coding-agent\/fixtures\/task\.json", "--source-root", "\/workspace\/src\/blazn", "--output", "\/workspace\/artifacts\/change\.patch"\]/);
});

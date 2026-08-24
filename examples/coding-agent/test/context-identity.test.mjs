import assert from "node:assert/strict";
import { cp, chmod, link, mkdtemp, rm, symlink, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { buildContextIdentity } from "../scripts/context-identity.mjs";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
async function copyContext() { const target=await mkdtemp(path.join(os.tmpdir(),"blazn-context-"));await cp(root,target,{recursive:true});return target; }

test("the effective Docker context is closed and reproducible", async () => {
  assert.equal(await buildContextIdentity(root), await buildContextIdentity(root));
  const target=await copyContext();
  try { await writeFile(path.join(target,"src/unbound.mjs"),"export default true;\n");await assert.rejects(()=>buildContextIdentity(target),/unexpected file/); }
  finally { await rm(target,{recursive:true,force:true}); }
});

test("context identity refuses symlinks, hard-link aliases, and executable mode drift", async () => {
  let target=await copyContext();
  try { const source=path.join(target,"src/coding-agent.mjs");await rm(source);await symlink("../package.json",source);await assert.rejects(()=>buildContextIdentity(target),/unsafe context file/); }
  finally { await rm(target,{recursive:true,force:true}); }
  target=await copyContext();
  try { const source=path.join(target,"src/coding-agent.mjs");await link(source,path.join(target,"hardlink-alias"));await assert.rejects(()=>buildContextIdentity(target),/unsafe context file/); }
  finally { await rm(target,{recursive:true,force:true}); }
  target=await copyContext();
  try { await chmod(path.join(target,"Dockerfile"),0o755);await assert.rejects(()=>buildContextIdentity(target),/unsafe context file/); }
  finally { await rm(target,{recursive:true,force:true}); }
});

test("context identity refuses symlinked parent directories", async () => {
  for (const directory of ["fixtures", "scripts", "test"]) {
    const target=await copyContext(), external=await mkdtemp(path.join(os.tmpdir(),"blazn-context-external-"));
    try {
      await cp(path.join(target,directory),external,{recursive:true});
      await rm(path.join(target,directory),{recursive:true,force:true});
      await symlink(external,path.join(target,directory));
      await assert.rejects(()=>buildContextIdentity(target),/unsafe context directory/);
    } finally { await rm(target,{recursive:true,force:true});await rm(external,{recursive:true,force:true}); }
  }
});

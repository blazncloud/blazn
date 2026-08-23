import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createRequire } from "node:module";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { Ajv2020 } from "ajv/dist/2020.js";
import type { FormatsPlugin } from "ajv-formats";
import { harnessSecretViolations, verifyAgentCompatibility, verifyHarnessBundle, verifyHarnessRun, verifyPortableEvaluation } from "./harness-contract.js";

const here=path.dirname(fileURLToPath(import.meta.url)),contracts=path.resolve(here,"../../../packages/contracts"),fixtures=path.join(contracts,"testdata/harness");
const require=createRequire(import.meta.url),formatsModule=require("ajv-formats") as {default?:FormatsPlugin}|FormatsPlugin,addFormats=("default" in formatsModule?formatsModule.default:formatsModule) as FormatsPlugin;
const json=async(file:string)=>JSON.parse(await readFile(file,"utf8")) as Record<string,unknown>;
function validate(schema:Record<string,unknown>,value:unknown){const ajv=new Ajv2020({allErrors:true,strict:false});addFormats(ajv);const fn=ajv.compile(schema);assert.equal(fn(value),true,JSON.stringify(fn.errors));}

test("Agent and four adapter profiles validate one shared resource model",async()=>{
  const agent=await json(path.join(fixtures,"agent-good.json")),agentSchema=await json(path.join(contracts,"agent.schema.json")),harnessSchema=await json(path.join(contracts,"harness.schema.json"));validate(agentSchema,agent);
  const bundles=[];for(const name of ["hermes-profile.json","codex-profile.json","claude-profile.json","generic-profile.json"]){const bundle=await json(path.join(fixtures,name));validate(harnessSchema,bundle);assert.deepEqual(verifyHarnessBundle(bundle),[],name);bundles.push(bundle);}
  assert.deepEqual(verifyAgentCompatibility(agent,bundles),[]);
  assert.match(verifyAgentCompatibility(agent,bundles.slice(0,3)).join(" "),/every allowed HarnessProfile/);
});

test("raw credentials, oversized overrides, undeclared credentials and capability mismatch fail before Sandbox",async()=>{
  const schema=await json(path.join(contracts,"harness.schema.json")),bad=await json(path.join(fixtures,"profile-bad-secret.json"));validate(schema,bad);assert.notDeepEqual(verifyHarnessBundle(bad),[]);
  const hermes=await json(path.join(fixtures,"hermes-profile.json")),secret=structuredClone(hermes);((secret.profile as Record<string,unknown>).overrides as Record<string,unknown>).authorization="Bearer forbidden-value";assert.ok(harnessSecretViolations(secret).length);
  const ajv=new Ajv2020({allErrors:true,strict:false});addFormats(ajv);const schemaValidator=ajv.compile(schema),oversized=structuredClone(hermes);((oversized.profile as Record<string,unknown>).overrides as Record<string,unknown>).format="x".repeat(513);assert.equal(schemaValidator(oversized),false,"oversized override passed schema");
  const undeclared=structuredClone(hermes);(undeclared.profile as Record<string,unknown>).credentials=[{capability:"model.anthropic",scope:"route:x",leaseSeconds:60}];assert.notDeepEqual(verifyHarnessBundle(undeclared),[]);
  const wrongScope=structuredClone(hermes);(wrongScope.profile as Record<string,unknown>).credentials=[{capability:"repository.read",scope:"route:not-a-repository",leaseSeconds:60}];assert.match(verifyHarnessBundle(wrongScope).join(" "),/scope/);
  const opaque=structuredClone(hermes);((opaque.profile as Record<string,unknown>).overrides as Record<string,unknown>).value="a".repeat(43);assert.match(verifyHarnessBundle(opaque).join(" "),/raw credential/);
  const agent=await json(path.join(fixtures,"agent-good.json")),limited=structuredClone(hermes);(limited.version as Record<string,unknown>).capabilities=["task.one-shot"];
  assert.match(verifyAgentCompatibility(agent,[limited]).join(" "),/missing capabilities/);
  const secretAgent=structuredClone(agent);(secretAgent.version as Record<string,unknown>).instructions="Use API_KEY=forbidden";assert.ok(verifyAgentCompatibility(secretAgent,[hermes]).some((error)=>error.includes("credential-like")));
  const run=await json(path.join(fixtures,"run-good.json")),incompatible=structuredClone(run),compatibility=incompatible.compatibility as Record<string,unknown>;compatibility.available=["task.one-shot"];compatibility.missing=["message.follow-up","conversation.resume","event.structured","event.streaming","output.patch","output.artifact","cancel.graceful"];compatibility.compatible=false;(incompatible.run as Record<string,unknown>).status="failed";(incompatible.run as Record<string,unknown>).sandboxId=null;(incompatible.result as Record<string,unknown>).status="failed";(incompatible.provenance as Record<string,unknown>).node=null;(incompatible.provenance as Record<string,unknown>).proxyDecision=null;validate(await json(path.join(contracts,"harness-run.schema.json")),incompatible);
  assert.doesNotMatch(verifyHarnessRun(agent,hermes,incompatible).join(" "),/before Sandbox/);
  (incompatible.run as Record<string,unknown>).sandboxId="32000000-0000-4000-8000-000000000001";assert.match(verifyHarnessRun(agent,hermes,incompatible).join(" "),/before Sandbox/);
});

test("normalized lifecycle, follow-up, resume, cancellation, provenance and terminal events are coherent",async()=>{
  const agent=await json(path.join(fixtures,"agent-good.json")),bundle=await json(path.join(fixtures,"hermes-profile.json")),run=await json(path.join(fixtures,"run-good.json")),schema=await json(path.join(contracts,"harness-run.schema.json"));validate(schema,run);assert.deepEqual(verifyHarnessRun(agent,bundle,run),[]);
  for(const mutate of [(v:Record<string,unknown>)=>{((v.events as Array<Record<string,unknown>>)[1]!).sequence=0;},(v:Record<string,unknown>)=>{((v.provenance as Record<string,Record<string,unknown>>).modelRoute!).version=2;},(v:Record<string,unknown>)=>{((v.session as Record<string,unknown>)).followUpCount=0;},(v:Record<string,unknown>)=>{((v.events as Array<Record<string,unknown>>).at(-1)!).type="harness.exited";},(v:Record<string,unknown>)=>{const event=(v.events as Array<Record<string,unknown>>)[2]!;event.type="tool.requested";event.payload={};}]){const candidate=structuredClone(run);mutate(candidate);assert.notDeepEqual(verifyHarnessRun(agent,bundle,candidate),[]);}
  const cancelled=structuredClone(run),events=cancelled.events as Array<Record<string,unknown>>,terminal=events.pop()!,base=structuredClone(events.at(-1)!);events.push({...base,id:"80000000-0000-4000-8000-000000000008",cursor:"cancel-request",type:"cancellation.requested",source:"control-plane",payload:{cancellationId:"37000000-0000-4000-8000-000000000001"},extensions:{}},{...base,id:"80000000-0000-4000-8000-000000000009",cursor:"cancel-ack",type:"cancellation.acknowledged",source:"adapter",payload:{cancellationId:"37000000-0000-4000-8000-000000000001"},extensions:{}},terminal);events.forEach((event,index)=>{event.sequence=index;});(cancelled.session as Record<string,unknown>).eventCursor=events.length-1;(cancelled.run as Record<string,unknown>).status="cancelled";(cancelled.result as Record<string,unknown>).status="cancelled";(terminal.payload as Record<string,unknown>).status="cancelled";(cancelled.session as Record<string,unknown>).cancellation={requested:true,acknowledged:true,processTreeTerminated:true,cleanupComplete:true};assert.deepEqual(verifyHarnessRun(agent,bundle,cancelled),[]);
  ((cancelled.session as Record<string,Record<string,unknown>>).cancellation!).processTreeTerminated=false;assert.match(verifyHarnessRun(agent,bundle,cancelled).join(" "),/termination/);
});

test("portable evaluation requires identical conformance cases and Artifact evidence",async()=>{
  const value=await json(path.join(fixtures,"evaluation-good.json")),schema=await json(path.join(contracts,"harness-conformance.schema.json"));validate(schema,value);assert.deepEqual(verifyPortableEvaluation(value),[]);
  for(const [kind,suffix] of [["hermes","1"],["codex-cli","2"],["claude-code","3"],["generic-cli","4"]]){const candidate=structuredClone(value);candidate.adapterKind=kind;candidate.harnessVersionId=`20000000-0000-4000-8000-00000000000${suffix}`;candidate.profileId=`21000000-0000-4000-8000-00000000000${suffix}`;validate(schema,candidate);assert.deepEqual(verifyPortableEvaluation(candidate),[]);}
  const missing=structuredClone(value);(missing.result as {caseResults:Array<unknown>}).caseResults.pop();assert.notDeepEqual(verifyPortableEvaluation(missing),[]);
});

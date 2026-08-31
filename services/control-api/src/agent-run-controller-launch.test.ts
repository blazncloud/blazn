import assert from "node:assert/strict";
import test from "node:test";
import { planAgentRunSandboxLaunch,type AgentRunSandboxAuthority,type HarnessWorkerReleaseAuthority } from "./agent-run-controller-launch.js";
import type { AgentRunWorkItem } from "./agent-run-controller-store.js";

const ids={run:"30000000-0000-4000-8000-000000000001",workspace:"40000000-0000-4000-8000-000000000001",
  project:"50000000-0000-4000-8000-000000000001",operation:"31000000-0000-4000-8000-000000000001",
  sandbox:"32000000-0000-4000-8000-000000000001",agent:"11000000-0000-4000-8000-000000000001",
  definition:"19000000-0000-4000-8000-000000000001",harness:"20000000-0000-4000-8000-000000000001",
  profile:"21000000-0000-4000-8000-000000000001",template:"60000000-0000-4000-8000-000000000001",
  route:"70000000-0000-4000-8000-000000000001",requester:"10000000-0000-4000-8000-000000000099"};
const digest=(pair:string)=>`sha256:${pair.repeat(32)}`;
const image=`registry.blazn.example.com/harness/hermes@${digest("12")}`;
const command=["/opt/blazn/blazn-harness-worker","--","/opt/blazn/hermes","run","--jsonl"];

function fixture(){
  const item:AgentRunWorkItem={runId:ids.run,workspaceId:ids.workspace,projectId:ids.project,runVersion:1,leaseToken:"91000000-0000-4000-8000-000000000001",
    leaseExpiresAt:"2026-08-31T12:01:00Z",attempt:1,requestedBy:ids.requester,planDigest:digest("23"),agentVersionId:ids.agent,
    agentVersionDigest:digest("34"),agentVersion:{id:ids.agent,digest:digest("34"),template:{versionId:ids.template,digest:digest("67")}},
    harnessDefinitionId:ids.definition,harnessVersionId:ids.harness,harnessVersionDigest:digest("45"),
    harnessVersion:{id:ids.harness,digest:digest("45"),implementation:{kind:"image",digest:image},executable:{path:"/opt/blazn/hermes",fixedArgv:["run","--jsonl"]},
      supportedPlatforms:["linux/amd64","linux/arm64"],provenance:{artifactDigest:digest("12")}},harnessProfileId:ids.profile,
    harnessProfileDigest:digest("56"),harnessProfile:{id:ids.profile,digest:digest("56"),harnessVersionId:ids.harness,status:"approved",
      model:{routeId:ids.route,routeVersion:1,protocol:"openai-responses"}},templateVersionId:ids.template,templateDigest:digest("67"),modelRouteId:ids.route,
    modelRouteVersion:1,modelProtocol:"openai-responses"};
  const release:HarnessWorkerReleaseAuthority={workerImage:image,workerExecutable:command[0]!,hermesIncluded:true,runnable:true,
    harnessVersionId:ids.harness,harnessVersionDigest:digest("45"),harnessExecutableDigest:digest("89")};
  const sandbox:AgentRunSandboxAuthority={templateVersionId:ids.template,templateDigest:digest("67"),templateName:"agent-hermes",
    templateVersion:"1.0.0",architecture:"amd64",imageDigest:image,command:[...command],operationId:ids.operation,sandboxId:ids.sandbox,
    expiresAt:"2026-08-31T13:00:00Z",listenerCredentialRef:"listener-token://75000000-0000-4000-8000-000000000001",
    listenerTokenFingerprint:digest("9a")};
  return{item,release,sandbox};
}

test("Agent Run launch plan binds the runnable image, reviewed argv, scope, and projections",()=>{
  const {item,release,sandbox}=fixture(),plan=planAgentRunSandboxLaunch(item,release,sandbox,new Date("2026-08-31T12:00:00Z"));
  assert.equal(plan.image,image);assert.deepEqual(plan.command,command);assert.equal(plan.template.id,ids.template);
  assert.deepEqual(plan.assignment.scope,{runId:ids.run,workspaceId:ids.workspace,projectId:ids.project,operationId:ids.operation,
    sandboxId:ids.sandbox,agentVersionId:ids.agent,agentVersionDigest:digest("34"),harnessProfileId:ids.profile,harnessProfileDigest:digest("56"),
    harnessVersionId:ids.harness,harnessVersionDigest:digest("45"),harnessExecutableDigest:digest("89"),routeId:ids.route,routeVersion:1,
    protocol:"openai-responses",expiresAt:"2026-08-31T13:00:00.000Z",listenerCredentialRef:sandbox.listenerCredentialRef,
    listenerTokenFingerprint:digest("9a")});
  assert.deepEqual(plan.projections.map(value=>[value.kind,value.path,value.readOnly]),[
    ["document","/run/blazn-harness/workload-scope.json",true],["listener-credential","/run/blazn-harness/listener-token",true],
    ["empty-directory","/workspace/artifacts",false]]);
  assert.equal(JSON.stringify(plan).includes("listenerToken\""),false);
});

test("Agent Run launch planning fails closed for foundation, placeholder, and mismatched releases",()=>{
  for(const mutate of [
    (value:ReturnType<typeof fixture>)=>{(value.release as {runnable:boolean}).runnable=false;},
    (value:ReturnType<typeof fixture>)=>{value.release.workerImage=`registry.blazn.invalid/harness/hermes@${digest("12")}`;},
    (value:ReturnType<typeof fixture>)=>{value.release.harnessExecutableDigest=digest("aa");},
    (value:ReturnType<typeof fixture>)=>{value.release.workerImage=`user:password@registry.blazn.example.com/harness/hermes@${digest("12")}`;},
    (value:ReturnType<typeof fixture>)=>{value.release.workerImage=`registry.blazn.example.com:65536/harness/hermes@${digest("12")}`;},
    (value:ReturnType<typeof fixture>)=>{value.release.workerImage=`registry.blazn.example.com /harness/hermes@${digest("12")}`;},
    (value:ReturnType<typeof fixture>)=>{value.sandbox.command=["/bin/sh","-c","id"];},
    (value:ReturnType<typeof fixture>)=>{value.sandbox.imageDigest=`registry.blazn.example.com/harness/other@${digest("ab")}`;},
    (value:ReturnType<typeof fixture>)=>{value.sandbox.expiresAt="2026-09-01T12:00:01Z";},
  ]){const value=fixture();mutate(value);assert.throws(()=>planAgentRunSandboxLaunch(value.item,value.release,value.sandbox,new Date("2026-08-31T12:00:00Z")));}
});

test("Agent Run launch planning rejects Harness metadata substitution",()=>{
  for(const mutate of [
    (item:AgentRunWorkItem)=>{(item.harnessVersion.implementation as Record<string,unknown>).digest=`registry.blazn.example.com/harness/other@${digest("ab")}`;},
    (item:AgentRunWorkItem)=>{(item.harnessVersion.executable as Record<string,unknown>).path="/bin/sh";},
    (item:AgentRunWorkItem)=>{(item.harnessVersion.executable as Record<string,unknown>).fixedArgv=["-c","id"];},
    (item:AgentRunWorkItem)=>{item.modelRouteVersion=2;},
  ]){const value=fixture();mutate(value.item);
    assert.throws(()=>planAgentRunSandboxLaunch(value.item,value.release,value.sandbox,new Date("2026-08-31T12:00:00Z")));}
});

test("Agent Run launch planning matches the worker UUID contract",()=>{
  for(const version of ["6","7","8"]){const value=fixture();
    value.sandbox.operationId=`31000000-0000-${version}000-8000-000000000001`;
    assert.throws(()=>planAgentRunSandboxLaunch(value.item,value.release,value.sandbox,new Date("2026-08-31T12:00:00Z")));}
});

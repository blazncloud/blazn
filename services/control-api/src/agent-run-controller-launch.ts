import type { AgentRunWorkItem } from "./agent-run-controller-store.js";

const workerExecutable="/opt/blazn/blazn-harness-worker";
const scopePath="/run/blazn-harness/workload-scope.json";
const listenerTokenPath="/run/blazn-harness/listener-token";
const artifactRoot="/workspace/artifacts";
const digestPattern=/^sha256:[0-9a-f]{64}$/;
const uuidPattern=/^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

export class AgentRunLaunchValidationError extends Error {
  constructor(readonly code:"launch_authority_invalid"|"launch_authority_mismatch"|"harness_release_unavailable",message:string){super(message);}
}

/** A controller-only, signed/published release observation. It is not accepted from a Run or user request. */
export interface HarnessWorkerReleaseAuthority {
  workerImage:string;
  workerExecutable:string;
  hermesIncluded:true;
  runnable:true;
  harnessVersionId:string;
  harnessVersionDigest:string;
  harnessExecutableDigest:string;
}

/** A lease-fenced resolution of the exact published SandboxTemplate version. */
export interface AgentRunSandboxAuthority {
  templateVersionId:string;
  templateDigest:string;
  templateName:string;
  templateVersion:string;
  architecture:"amd64"|"arm64";
  imageDigest:string;
  command:string[];
  operationId:string;
  sandboxId:string;
  expiresAt:string;
  listenerCredentialRef:string;
  listenerTokenFingerprint:string;
}

export interface AgentRunSandboxLaunchPlan {
  image:string;
  command:string[];
  architecture:"amd64"|"arm64";
  template:{id:string;digest:string;name:string;version:string};
  assignment:{schemaVersion:"blazn.dev/harness-worker/v1alpha1";type:"execute";scope:Record<string,unknown>};
  projections:readonly [
    {kind:"document";path:typeof scopePath;readOnly:true;content:"assignment.scope"},
    {kind:"listener-credential";path:typeof listenerTokenPath;readOnly:true;reference:string},
    {kind:"empty-directory";path:typeof artifactRoot;readOnly:false},
  ];
}

/**
 * Produces inert launch data only. Allocation remains forbidden until the
 * Sandbox controller can project the returned scope and one-shot listener
 * credential without placing credential bytes in a CRD, template, or event.
 */
export function planAgentRunSandboxLaunch(item:AgentRunWorkItem,release:HarnessWorkerReleaseAuthority,
  sandbox:AgentRunSandboxAuthority,now:Date):AgentRunSandboxLaunchPlan {
  validateClaimDocuments(item);
  validateRelease(release,item);
  validateSandbox(sandbox,item,release,now);
  const harness=record(item.harnessVersion,"HarnessVersion"),executable=record(harness.executable,"Harness executable");
  const path=text(executable.path,"Harness executable path"),fixedArgv=strings(executable.fixedArgv,"Harness fixed argv");
  if(path!=="/opt/blazn/hermes"||fixedArgv.length!==2||fixedArgv[0]!=="run"||fixedArgv[1]!=="--jsonl")
    mismatch("Harness executable is not the reviewed Hermes command");
  const command=[workerExecutable,"--",path,...fixedArgv];
  if(!equal(command,sandbox.command))mismatch("SandboxTemplate command does not match the reviewed worker command");
  const scope={runId:item.runId,workspaceId:item.workspaceId,projectId:item.projectId,operationId:sandbox.operationId,
    sandboxId:sandbox.sandboxId,agentVersionId:item.agentVersionId,agentVersionDigest:item.agentVersionDigest,
    harnessProfileId:item.harnessProfileId,harnessProfileDigest:item.harnessProfileDigest,harnessVersionId:item.harnessVersionId,
    harnessVersionDigest:item.harnessVersionDigest,harnessExecutableDigest:release.harnessExecutableDigest,
    routeId:item.modelRouteId,routeVersion:item.modelRouteVersion,protocol:item.modelProtocol,expiresAt:new Date(sandbox.expiresAt).toISOString(),
    listenerCredentialRef:sandbox.listenerCredentialRef,listenerTokenFingerprint:sandbox.listenerTokenFingerprint};
  return{image:release.workerImage,command,architecture:sandbox.architecture,
    template:{id:sandbox.templateVersionId,digest:sandbox.templateDigest,name:sandbox.templateName,version:sandbox.templateVersion},
    assignment:{schemaVersion:"blazn.dev/harness-worker/v1alpha1",type:"execute",scope},
    projections:[{kind:"document",path:scopePath,readOnly:true,content:"assignment.scope"},
      {kind:"listener-credential",path:listenerTokenPath,readOnly:true,reference:sandbox.listenerCredentialRef},
      {kind:"empty-directory",path:artifactRoot,readOnly:false}]};
}

function validateClaimDocuments(item:AgentRunWorkItem){
  const agent=record(item.agentVersion,"AgentVersion"),template=record(agent.template,"AgentVersion template");
  if(agent.id!==item.agentVersionId||agent.digest!==item.agentVersionDigest||template.versionId!==item.templateVersionId||template.digest!==item.templateDigest)
    mismatch("claimed AgentVersion document does not match its frozen binding");
  const harness=record(item.harnessVersion,"HarnessVersion");
  if(harness.id!==item.harnessVersionId||harness.digest!==item.harnessVersionDigest)
    mismatch("claimed HarnessVersion document does not match its frozen binding");
  const profile=record(item.harnessProfile,"HarnessProfile"),model=record(profile.model,"HarnessProfile model");
  if(profile.id!==item.harnessProfileId||profile.digest!==item.harnessProfileDigest||profile.harnessVersionId!==item.harnessVersionId||profile.status!=="approved"||
    model.routeId!==item.modelRouteId||model.routeVersion!==item.modelRouteVersion||model.protocol!==item.modelProtocol)
    mismatch("claimed HarnessProfile document does not match its frozen binding");
}

function validateRelease(value:HarnessWorkerReleaseAuthority,item:AgentRunWorkItem){
  if(value.hermesIncluded!==true||value.runnable!==true||value.workerExecutable!==workerExecutable||!immutableImage(value.workerImage)||!uuidPattern.test(value.harnessVersionId)||
    !digestPattern.test(value.harnessVersionDigest)||!realDigest(value.harnessExecutableDigest))unavailable("Harness worker release authority is incomplete");
  if(value.harnessVersionId!==item.harnessVersionId||value.harnessVersionDigest!==item.harnessVersionDigest)
    mismatch("Harness worker release authority does not match the claimed HarnessVersion");
  const harness=record(item.harnessVersion,"HarnessVersion"),implementation=record(harness.implementation,"Harness implementation"),provenance=record(harness.provenance,"Harness provenance");
  if(implementation.kind!=="image"||implementation.digest!==value.workerImage)mismatch("Harness implementation does not match the worker image");
  const imageDigest=`sha256:${value.workerImage.slice(value.workerImage.lastIndexOf("@sha256:")+8)}`;
  if(provenance.artifactDigest!==imageDigest)mismatch("Harness provenance does not bind the worker image");
}

function validateSandbox(value:AgentRunSandboxAuthority,item:AgentRunWorkItem,release:HarnessWorkerReleaseAuthority,now:Date){
  if(!uuidPattern.test(value.operationId)||!uuidPattern.test(value.sandboxId)||!uuidPattern.test(value.templateVersionId)||
    !digestPattern.test(value.templateDigest)||!name(value.templateName)||!version(value.templateVersion)||
    !["amd64","arm64"].includes(value.architecture)||!immutableImage(value.imageDigest)||!Array.isArray(value.command)||
    !value.command.every(part=>typeof part==="string"&&part.length>0&&part.length<=1024&&!part.includes("\0"))||
    !/^listener-token:\/\/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(value.listenerCredentialRef)||!realDigest(value.listenerTokenFingerprint))invalid("Sandbox launch authority is invalid");
  if(value.templateVersionId!==item.templateVersionId||value.templateDigest!==item.templateDigest)mismatch("SandboxTemplate authority does not match the claimed template");
  if(value.imageDigest!==release.workerImage)mismatch("SandboxTemplate image does not match the worker release");
  const expires=new Date(value.expiresAt),maximum=new Date(now.valueOf()+24*60*60*1000);
  if(Number.isNaN(now.valueOf())||Number.isNaN(expires.valueOf())||expires<=now||expires>maximum)invalid("Sandbox launch expiry is invalid");
  const definition=record(item.harnessVersion,"HarnessVersion"),platforms=strings(definition.supportedPlatforms,"Harness supported platforms");
  if(!platforms.includes(`linux/${value.architecture}`))mismatch("Sandbox architecture is unsupported by the HarnessVersion");
}

function immutableImage(value:string){
  if(typeof value!=="string"||value.length>512||value!==value.toLowerCase())return false;
  const match=/^([^/]+)\/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:\/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*@sha256:([0-9a-f]{64})$/.exec(value);
  if(!match)return false;const host=match[1]!,digest=match[2]!;
  return host.includes(".")&&!host.endsWith(".invalid")&&!host.endsWith(".test")&&!host.endsWith(".example")&&!/^([0-9a-f])\1{63}$/.test(digest);
}
function realDigest(value:string){return digestPattern.test(value)&&!/^sha256:([0-9a-f])\1{63}$/.test(value);}
function record(value:unknown,label:string):Record<string,unknown>{if(!value||typeof value!=="object"||Array.isArray(value))invalid(`${label} is invalid`);return value as Record<string,unknown>;}
function text(value:unknown,label:string){if(typeof value!=="string")invalid(`${label} is invalid`);return value;}
function strings(value:unknown,label:string){if(!Array.isArray(value)||!value.every(entry=>typeof entry==="string"))invalid(`${label} is invalid`);return value as string[];}
function name(value:string){return /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(value);}
function version(value:string){return /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(value);}
function equal(left:string[],right:string[]){return left.length===right.length&&left.every((value,index)=>value===right[index]);}
function invalid(message:string):never{throw new AgentRunLaunchValidationError("launch_authority_invalid",message);}
function mismatch(message:string):never{throw new AgentRunLaunchValidationError("launch_authority_mismatch",message);}
function unavailable(message:string):never{throw new AgentRunLaunchValidationError("harness_release_unavailable",message);}

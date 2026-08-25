import { createHash } from "node:crypto";
import { spawn } from "node:child_process";
import { mkdtemp,readFile,rm,stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { DevelopmentControllerWorkItem } from "./development-controller-store.js";
import { resolveDevelopmentImageChildren,type DevelopmentImageChildren } from "./development-registry-resolver.js";

const digestPattern=/^sha256:[0-9a-f]{64}$/;
const uuidPattern=/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
export const qualifiedBuildKit={imageDigest:"moby/buildkit@sha256:79cc6476ab1a3371c9afd8b44e7c55610057c43e18d9b39b68e2b0c2475cc1b6",version:"v0.25.1"} as const;

export interface DevelopmentArtifactPayload {id:string;role:string;kind:string;contentDigest:string;content:Buffer}
export interface DevelopmentExecutionResult {
  execution:{nodeId:string;sandboxId:string};
  document:Record<string,unknown>;
  artifacts:DevelopmentArtifactPayload[];
}
export interface DevelopmentBuildExecutor {execute(item:DevelopmentControllerWorkItem,signal:AbortSignal):Promise<DevelopmentExecutionResult>}

export interface BuildKitExecutorConfig {
  buildctlPath:string;address:string;serverName:string;builderId:string;caPath:string;certificatePath:string;keyPath:string;
  evidenceCommand:string;maximumArtifactBytes:number;maximumTotalArtifactBytes:number;executionTimeoutSeconds:number;
  evidenceSecretsRoot:string;registryAuthority:string;
}

interface CommandResult {code:number;logDigest:string}
type CommandRunner=(file:string,args:string[],options:{input?:Buffer;env?:NodeJS.ProcessEnv;signal:AbortSignal;killGraceMilliseconds?:number})=>Promise<CommandResult&{stdout?:Buffer}>;
type ImageResolver=(reference:string,signal:AbortSignal,options:{allowedAuthority:string})=>Promise<DevelopmentImageChildren>;

export class BuildKitDevelopmentExecutor implements DevelopmentBuildExecutor {
  constructor(private readonly config:BuildKitExecutorConfig,private readonly run:CommandRunner=runDevelopmentCommand,
    private readonly resolveImages:ImageResolver=resolveDevelopmentImageChildren){}
  async execute(item:DevelopmentControllerWorkItem,signal:AbortSignal):Promise<DevelopmentExecutionResult>{
    const project=projectBuild(item.projectSnapshot),artifactIds=developmentArtifactIds(item);
    if(!uuidPattern.test(this.config.builderId))throw new Error("BuildKit builder ID is invalid");
    const deadline=AbortSignal.timeout(this.config.executionTimeoutSeconds*1000),executionSignal=AbortSignal.any([signal,deadline]);
    const directory=await mkdtemp(join(tmpdir(),"blazn-development-"));
    const metadataPath=join(directory,"build-metadata.json"),startedAt=new Date().toISOString();
    try{
      const args=buildctlArguments(item,project,metadataPath,this.config);
      const build=await this.run(this.config.buildctlPath,args,{signal:executionSignal,env:safeEnvironment({BUILDKIT_HOST:this.config.address})});
      // A nonzero build has no observed immutable image result. Retry it as an
      // operational failure instead of allowing a fabricated terminal output.
      if(build.code!==0)throw new Error("BuildKit build failed");
      const completedAt=new Date().toISOString();
      const metadata=JSON.parse(await readBoundedFile(metadataPath,1024*1024)) as unknown;
      const imageIndexDigest=metadataImageDigest(metadata);
      const imageIndexReference=`${project.registryRepository}@${imageIndexDigest}`;
      const imageChildren=await this.resolveImages(imageIndexReference,executionSignal,{allowedAuthority:this.config.registryAuthority});
      const collectorInput=Buffer.from(JSON.stringify({
        schemaVersion:"blazn.dev/buildkit-execution/v1alpha1",workItem:safeWorkItem(item),artifactIds,
        builder:{id:this.config.builderId,profile:text(record(item.projectSnapshot.policy)?.builderProfile),...qualifiedBuildKit},
        build:{succeeded:true,imageIndexDigest,imageChildren,logDigest:build.logDigest,startedAt,completedAt},
      }));
      const collected=await this.run(this.config.evidenceCommand,[],{input:collectorInput,signal:executionSignal,env:safeEnvironment({BLAZN_DEVELOPMENT_SECRETS_ROOT:this.config.evidenceSecretsRoot})});
      if(collected.code!==0||!collected.stdout)throw new Error("Development evidence collector failed");
      const profile=text(record(item.projectSnapshot.policy)?.builderProfile);
      return parseCollectorResult(collected.stdout,item,artifactIds,this.config,{imageIndexReference,imageChildren,builder:{id:this.config.builderId,profile,...qualifiedBuildKit}});
    }finally{await rm(directory,{recursive:true,force:true});}
  }
}

interface ProjectBuild {context:string;dockerfile:string;registryRepository:string;platforms:string[]}
function projectBuild(snapshot:Record<string,unknown>):ProjectBuild{
  const build=record(snapshot.build),platforms=snapshot.platforms;
  const context=text(build?.context),dockerfile=text(build?.dockerfile),registryRepository=text(build?.registryRepository);
  if(!context||!dockerfile||!registryRepository||!Array.isArray(platforms)||platforms.length!==2||
    platforms.some(value=>value!=="linux/amd64"&&value!=="linux/arm64")||new Set(platforms).size!==2)throw new Error("DevelopmentProject build input is invalid");
  return{context,dockerfile,registryRepository,platforms:platforms as string[]};
}

export function buildctlArguments(item:DevelopmentControllerWorkItem,project:ProjectBuild,metadataPath:string,config:BuildKitExecutorConfig):string[]{
  for(const path of [config.buildctlPath,config.caPath,config.certificatePath,config.keyPath])if(!path.startsWith("/"))throw new Error("BuildKit executable and TLS paths must be absolute");
  if(!config.evidenceCommand.startsWith("/"))throw new Error("Development evidence command must be absolute");
  if(!config.evidenceSecretsRoot.startsWith("/")||config.evidenceSecretsRoot.includes("\0"))throw new Error("Development evidence secrets root must be absolute");
  let repository:URL;try{repository=new URL(item.source.repository);}catch{throw new Error("Development source identity is invalid");}
  if(repository.protocol!=="https:"||repository.username||repository.password||repository.search||repository.hash||repository.href!==item.source.repository||!/^[0-9a-f]{40}(?:[0-9a-f]{24})?$/.test(item.source.commit))throw new Error("Development source identity is invalid");
  let address:URL;try{address=new URL(config.address);}catch{throw new Error("BuildKit address is invalid");}
  if(address.protocol!=="tcp:"||!address.hostname||!address.port||address.username||address.password||address.pathname||address.search||address.hash||!/^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$/.test(config.serverName))throw new Error("BuildKit address is invalid");
  const target=`${project.registryRepository}:build-${item.buildId}`;
  const source=`${item.source.repository}#${item.source.commit}${project.context==="."?"":`:${project.context}`}`;
  const args=["--tlscacert",config.caPath,"--tlscert",config.certificatePath,"--tlskey",config.keyPath,"--tlservername",config.serverName,
    "build","--frontend","dockerfile.v0","--opt",`context=${source}`,
    "--opt",`filename=${project.dockerfile}`,"--opt",`platform=${project.platforms.join(",")}`];
  args.push("--output",`type=image,name=${target},push=true`,"--metadata-file",metadataPath);
  return args;
}

export function metadataImageDigest(value:unknown):string{
  const root=record(value),digest=text(root?.["containerimage.digest"]);
  if(!digestPattern.test(digest))throw new Error("BuildKit metadata lacks an immutable image index digest");
  return digest;
}

export function developmentArtifactIds(item:DevelopmentControllerWorkItem):Record<string,string>{
  const project=item.projectSnapshot,tests=record(project.tests)??{},platforms=Array.isArray(project.platforms)?project.platforms:[];
  const roles=[...platforms.map(value=>`refresh/${String(value)}`),"provenance","signature",
    ...Object.keys(tests).sort().map(name=>`project-test/${name}`),
    ...platforms.map(value=>`security/${String(value)}`),...platforms.map(value=>`lifecycle/${String(value)}`),
    "cleanup","reproducibility-comparison"];
  return Object.fromEntries(roles.map(role=>[role,deterministicUUID(item.buildId,role)]));
}

function deterministicUUID(buildId:string,role:string):string{
  const bytes=createHash("sha256").update("blazn-development-artifact-v1\0").update(buildId).update("\0").update(role).digest().subarray(0,16);
  bytes[6]=(bytes[6]!&0x0f)|0x80;bytes[8]=(bytes[8]!&0x3f)|0x80;
  const hex=bytes.toString("hex");return`${hex.slice(0,8)}-${hex.slice(8,12)}-${hex.slice(12,16)}-${hex.slice(16,20)}-${hex.slice(20)}`;
}

function safeWorkItem(item:DevelopmentControllerWorkItem){return{buildId:item.buildId,workspaceId:item.workspaceId,projectId:item.projectId,runId:item.runId,buildVersion:item.buildVersion,generation:item.generation,requestedBy:item.requestedBy,source:item.source,projectManifestDigest:item.projectManifestDigest,projectSnapshot:item.projectSnapshot,planDigest:item.planDigest,createdAt:item.createdAt};}
function safeEnvironment(extra:NodeJS.ProcessEnv={}):NodeJS.ProcessEnv{return{PATH:"/usr/local/bin:/usr/bin:/bin",HOME:"/tmp",LANG:"C.UTF-8",...extra};}

interface ActualBuild {imageIndexReference:string;imageChildren:DevelopmentImageChildren;builder:{id:string;profile:string;imageDigest:string;version:string}}
export function parseCollectorResult(raw:Buffer,item:DevelopmentControllerWorkItem,ids:Record<string,string>,config:Pick<BuildKitExecutorConfig,"maximumArtifactBytes"|"maximumTotalArtifactBytes">,actual:ActualBuild):DevelopmentExecutionResult{
  if(raw.byteLength>4*1024*1024)throw new Error("Development evidence response is too large");
  let value:unknown;try{value=JSON.parse(raw.toString("utf8"));}catch{throw new Error("Development evidence response is invalid JSON");}
  const root=record(value),execution=record(root?.execution),document=record(root?.document),artifacts=root?.artifacts;
  if(!execution||!uuidPattern.test(text(execution.nodeId))||!uuidPattern.test(text(execution.sandboxId))||!document||!Array.isArray(artifacts)||artifacts.length<1||artifacts.length>100)throw new Error("Development evidence response shape is invalid");
  const expected=new Map(Object.entries(ids)),seen=new Set<string>();let total=0;
  const parsed=artifacts.map((rawArtifact):DevelopmentArtifactPayload=>{
    const artifact=record(rawArtifact),role=text(artifact?.role),id=text(artifact?.id),kind=text(artifact?.kind),claimed=text(artifact?.contentDigest),encoded=text(artifact?.contentBase64);
    if(!artifact||expected.get(role)!==id||seen.has(role)||!/^development\.[a-z]+$/.test(kind)||!digestPattern.test(claimed)||!/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(encoded))throw new Error("Development evidence Artifact identity is invalid");
    seen.add(role);const content=Buffer.from(encoded,"base64");total+=content.byteLength;
    if(content.byteLength<1||content.byteLength>config.maximumArtifactBytes||total>config.maximumTotalArtifactBytes)throw new Error("Development evidence Artifact exceeds its size limit");
    const digest=`sha256:${createHash("sha256").update(content).digest("hex")}`;
    if(digest!==claimed)throw new Error("Development evidence Artifact digest is invalid");
    validateEvidenceContent(content,role);
    return{id,role,kind,contentDigest:digest,content};
  });
  if(seen.size!==expected.size||[...expected.keys()].some(role=>!seen.has(role)))throw new Error("Development evidence Artifact roles are incomplete");
  if(document.id!==item.buildId||document.workspaceId!==item.workspaceId||document.projectId!==item.projectId||document.runId!==item.runId)throw new Error("Development evidence document identity is invalid");
  const outputs=record(document.outputs),builder=record(document.builder),status=text(document.status);
  if(status!=="succeeded"||!outputs||outputs.imageIndexDigest!==actual.imageIndexReference)throw new Error("Development evidence success is not bound to BuildKit output");
  const images=Array.isArray(outputs.images)?outputs.images:[],refresh=record(outputs.refreshArtifacts),observed=new Map<string,string>();
  for(const rawImage of images){const entry=record(rawImage),platform=text(entry?.platform),digest=text(entry?.digest);if(!entry||observed.has(platform))throw new Error("Development evidence image children are not bound to the resolved index");observed.set(platform,digest);}
  for(const platform of ["linux/amd64","linux/arm64"] as const){
    const refreshEntry=record(refresh?.[platform]);
    if(observed.get(platform)!==actual.imageChildren[platform]||text(refreshEntry?.imageDigest)!==actual.imageChildren[platform])throw new Error("Development evidence image children are not bound to the resolved index");
  }
  if(images.length!==2||observed.size!==2)throw new Error("Development evidence image children are not bound to the resolved index");
  if(!builder||builder.id!==actual.builder.id||builder.profile!==actual.builder.profile||builder.imageDigest!==actual.builder.imageDigest||builder.version!==actual.builder.version)throw new Error("Development evidence builder is not bound to the qualified BuildKit identity");
  return{execution:{nodeId:text(execution.nodeId),sandboxId:text(execution.sandboxId)},document,artifacts:parsed};
}

function validateEvidenceContent(content:Buffer,role:string){
  let value:unknown,textValue:string;try{textValue=new TextDecoder("utf-8",{fatal:true}).decode(content);value=JSON.parse(textValue);}catch{throw new Error(`Development evidence Artifact ${role} is not UTF-8 JSON`);}
  if(!value||typeof value!=="object"||Array.isArray(value)||canonicalJSON(value)!==textValue)throw new Error(`Development evidence Artifact ${role} is not canonical JSON or contains duplicate keys`);
  const violation=secretViolation(value);if(violation)throw new Error(`Development evidence Artifact ${role} contains forbidden ${violation}`);
}
function secretViolation(value:unknown):string|undefined{
  if(typeof value==="string"){
    if(/(?:^|[?&])(?:x-amz-signature|x-goog-signature|signature|sig|token|access_token|api_key|credential)=/i.test(value))return"signed URL";
    if(/\b(?:bearer|basic)\s+[a-z0-9+/_=-]+/i.test(value)||/:\/\/[^/\s]+@/.test(value))return"credential";
    if(/^(?:tcp|https?):\/\/[^\s]*buildkit/i.test(value)||/^(?:unix|npipe):\/\//i.test(value))return"BuildKit address";
    if(/\b(?:sk-(?:proj-|svcacct-|ant-)?[A-Za-z0-9_-]{16,}|github_pat_[A-Za-z0-9_]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|(?:AKIA|ASIA)[A-Z0-9]{16}|xox[baprs]-[A-Za-z0-9-]{10,}|hf_[A-Za-z0-9]{20,}|npm_[A-Za-z0-9]{20,}|(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,})\b/.test(value))return"provider token";
    if(/-----BEGIN (?:[A-Z0-9 ]+ )?PRIVATE KEY-----/.test(value))return"private key";
    if(/\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b/.test(value))return"JWT";
    if(/\b(?:api[_-]?key|access[_-]?token|refresh[_-]?token|password|secret|authorization|credential)\s*[:=]\s*[^\s,;]{4,}/i.test(value))return"credential assignment";
  }else if(Array.isArray(value)){for(const child of value){const found=secretViolation(child);if(found)return found;}}
  else if(value&&typeof value==="object")for(const [key,child] of Object.entries(value)){
    const canonical=key.replaceAll("_","").replaceAll("-","").toLowerCase();
    if(["authorization","credential","credentials","password","secret","secrets","token","accesstoken","refreshtoken","apikey","objectkey","signedurl","buildkitendpoint","buildkitclientcertificate","registrycredential"].includes(canonical))return key;
    const found=secretViolation(child);if(found)return found;
  }
  return undefined;
}
function canonicalJSON(value:unknown):string{if(value===null||typeof value==="string"||typeof value==="boolean")return JSON.stringify(value);if(typeof value==="number"){if(!Number.isFinite(value))throw new Error("non-finite JSON number");return JSON.stringify(value);}if(Array.isArray(value))return`[${value.map(canonicalJSON).join(",")}]`;if(value&&typeof value==="object")return`{${Object.keys(value).sort().map(key=>`${JSON.stringify(key)}:${canonicalJSON((value as Record<string,unknown>)[key])}`).join(",")}}`;throw new Error("invalid JSON value");}

async function readBoundedFile(path:string,limit:number):Promise<string>{const info=await stat(path);if(!info.isFile()||info.size<2||info.size>limit)throw new Error("BuildKit metadata file is invalid");return readFile(path,"utf8");}
export async function runDevelopmentCommand(file:string,args:string[],options:{input?:Buffer;env?:NodeJS.ProcessEnv;signal:AbortSignal;killGraceMilliseconds?:number}):Promise<CommandResult&{stdout?:Buffer}>{
  return new Promise((resolve,reject)=>{
    const child=spawn(file,args,{env:options.env,signal:options.signal,stdio:[options.input?"pipe":"ignore","pipe","pipe"]}),hash=createHash("sha256"),stdout:Buffer[]=[];let stdoutBytes=0,escalation:NodeJS.Timeout|undefined;
    const abort=()=>{escalation=setTimeout(()=>child.kill("SIGKILL"),options.killGraceMilliseconds??5000);escalation.unref();};options.signal.addEventListener("abort",abort,{once:true});if(options.signal.aborted)abort();
    child.stdout!.on("data",(chunk:Buffer)=>{stdoutBytes+=chunk.length;if(stdoutBytes<=4*1024*1024)stdout.push(chunk);});
    child.stderr!.on("data",(chunk:Buffer)=>hash.update(chunk));child.once("error",error=>{if(!(error instanceof Error&&error.name==="AbortError"))reject(error);});child.once("close",code=>{options.signal.removeEventListener("abort",abort);if(escalation)clearTimeout(escalation);resolve({code:code??1,logDigest:`sha256:${hash.digest("hex")}`,...(stdoutBytes<=4*1024*1024?{stdout:Buffer.concat(stdout)}:{})});});
    if(options.input)child.stdin!.end(options.input);
  });
}
function record(value:unknown):Record<string,unknown>|undefined{return value!==null&&typeof value==="object"&&!Array.isArray(value)?value as Record<string,unknown>:undefined;}
function text(value:unknown):string{return typeof value==="string"?value:"";}

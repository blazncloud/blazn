import type { Database } from "./db.js";

export interface DevelopmentSandboxTestRun {
  buildId:string;workspaceId:string;projectId:string;platform:"linux/amd64"|"linux/arm64";testName:string;sandboxId:string;
  status:string;sandboxState:string;candidateImageIndex:string;candidateImageBound:boolean;argv:string[];argvDigest:string;
  timeoutSeconds:number;backendUid?:string;backendResourceVersion?:string;podNamespace?:string;podName?:string;podUid?:string;
  podResourceVersion?:string;observationDigest?:string;nodeId?:string;receipt?:Record<string,unknown>;
}

export interface DevelopmentSandboxEvidenceStore {
  prepare(buildId:string,generation:number,platform:string,testName:string,candidateImageIndex:string):Promise<DevelopmentSandboxTestRun|undefined>;
  resolve(buildId:string,generation:number,platform:string,testName:string):Promise<DevelopmentSandboxTestRun|undefined>;
  authorizeExecution(buildId:string,generation:number,platform:string,testName:string,sandboxId:string):Promise<boolean>;
}

export interface DevelopmentSandboxCommandTransport {
  execute(run:DevelopmentSandboxTestRun,signal:AbortSignal):Promise<{remoteExitCode:number;stdoutDigest:string;stderrDigest:string;stdoutBytes:number;stderrBytes:number}>;
}

export class PgDevelopmentSandboxEvidenceStore implements DevelopmentSandboxEvidenceStore {
  constructor(private readonly database:Database){}
  async prepare(buildId:string,generation:number,platform:string,testName:string,candidateImageIndex:string){
    const result=await this.database.query("SELECT * FROM development_collector_prepare_sandbox_v1($1,$2,$3,$4,$5)",[buildId,generation,platform,testName,candidateImageIndex]);
    return result.rows[0]?testRun(result.rows[0]):undefined;
  }
  async resolve(buildId:string,generation:number,platform:string,testName:string){
    const result=await this.database.query("SELECT * FROM development_collector_resolve_sandbox_v1($1,$2,$3,$4)",[buildId,generation,platform,testName]);
    return result.rows[0]?testRun(result.rows[0]):undefined;
  }
  async authorizeExecution(buildId:string,generation:number,platform:string,testName:string,sandboxId:string){const result=await this.database.query<{authorized:boolean}>("SELECT development_collector_authorize_execution_v1($1,$2,$3,$4,$5) AS authorized",[buildId,generation,platform,testName,sandboxId]);return result.rows[0]?.authorized===true;}
}

export interface DevelopmentSandboxEvidenceInput {
  workItem:{buildId:string;generation:number;projectSnapshot:Record<string,unknown>};
  build:{imageIndexDigest:string};
}

export class DevelopmentSandboxEvidenceOrchestrator {
  constructor(private readonly store:DevelopmentSandboxEvidenceStore,private readonly transport:DevelopmentSandboxCommandTransport,
    private readonly pollMilliseconds=1000){}
  async execute(input:DevelopmentSandboxEvidenceInput,signal:AbortSignal):Promise<never>{
    const buildId=input.workItem.buildId,generation=input.workItem.generation,tests=record(input.workItem.projectSnapshot.tests),build=record(input.workItem.projectSnapshot.build);
    const repository=text(build?.registryRepository),digest=input.build.imageIndexDigest,imageIndex=`${repository}@${digest}`;
    if(!uuidPattern.test(buildId)||!Number.isSafeInteger(generation)||generation<1||!digestPattern.test(digest)||!ociPattern.test(imageIndex)||!tests||Object.keys(tests).length<1)throw new Error("Development Sandbox evidence input is invalid");
    const matrix=(['linux/amd64','linux/arm64'] as const).flatMap(platform=>Object.keys(tests).sort().map(testName=>({platform,testName})));
    for(const {platform,testName} of matrix){
      const prepared=await this.store.prepare(buildId,generation,platform,testName,imageIndex);
      if(!prepared)throw new Error("Development Sandbox preparation was fenced");
    }
    for(const {platform,testName} of matrix){
      const ready=await this.waitReady(buildId,generation,platform,testName,signal);
      // Ordinary Sandbox rows remain bound to their published template image.
      // The candidate-binding migration must prove the exact BuildKit index and
      // architecture child before any committed argv may execute.
      if(!ready.candidateImageBound)throw new Error("Development candidate image is not bound to the Sandbox");
      if(!await this.store.authorizeExecution(buildId,generation,platform,testName,ready.sandboxId))throw new Error("Development Sandbox execution was fenced");
      await this.transport.execute(ready,signal);
    }
    throw new Error("Development Gate-6 evidence aggregation is incomplete");
  }
  private async waitReady(buildId:string,generation:number,platform:string,testName:string,signal:AbortSignal){
    for(;;){
      if(signal.aborted)throw signal.reason;
      const run=await this.store.resolve(buildId,generation,platform,testName);
      if(!run)throw new Error("Development Sandbox evidence state disappeared");
      if(run.sandboxState==="failed"||run.sandboxState==="deleted")throw new Error("Development Sandbox did not become ready");
      if(run.sandboxState==="ready"||run.sandboxState==="running"){
        if(!run.backendUid||!run.backendResourceVersion||!run.podNamespace||!run.podName||!run.podUid||!run.podResourceVersion||!run.observationDigest)
          throw new Error("Development Sandbox readiness lacks frozen admission evidence");
        return run;
      }
      await abortableDelay(this.pollMilliseconds,signal);
    }
  }
}

function testRun(row:Record<string,unknown>):DevelopmentSandboxTestRun{return{
  buildId:text(row.build_id),workspaceId:text(row.workspace_id),projectId:text(row.project_id),platform:text(row.platform) as DevelopmentSandboxTestRun["platform"],
  testName:text(row.test_name),sandboxId:text(row.sandbox_id),status:text(row.status),sandboxState:text(row.sandbox_state),candidateImageIndex:text(row.candidate_image_index),
  candidateImageBound:row.candidate_image_bound===true,argv:Array.isArray(row.argv)?row.argv.map(text):[],argvDigest:text(row.argv_digest),timeoutSeconds:Number(row.timeout_seconds),
  ...optional("backendUid",row.backend_uid),...optional("backendResourceVersion",row.backend_resource_version),...optional("podNamespace",row.pod_namespace),
  ...optional("podName",row.pod_name),...optional("podUid",row.pod_uid),...optional("podResourceVersion",row.pod_resource_version),
  ...optional("observationDigest",row.observation_digest),...optional("nodeId",row.node_id),...(record(row.receipt)?{receipt:record(row.receipt)!}:{}),
};}
function abortableDelay(milliseconds:number,signal:AbortSignal){return new Promise<void>((resolve,reject)=>{const timer=setTimeout(done,milliseconds);function done(){signal.removeEventListener("abort",abort);resolve();}function abort(){clearTimeout(timer);reject(signal.reason);}signal.addEventListener("abort",abort,{once:true});});}
function optional(key:string,value:unknown){return typeof value==="string"&&value?{[key]:value}:{};}
function text(value:unknown){return typeof value==="string"?value:"";}
function record(value:unknown):Record<string,unknown>|undefined{return value&&typeof value==="object"&&!Array.isArray(value)?value as Record<string,unknown>:undefined;}
const uuidPattern=/^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const digestPattern=/^sha256:[0-9a-f]{64}$/;
const ociPattern=/^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?(?::[0-9]{1,5})?\/[a-z0-9][a-z0-9._/-]{0,510}@sha256:[0-9a-f]{64}$/;

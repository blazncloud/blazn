import { readFile } from "node:fs/promises";
import { createDatabase } from "./db.js";
import { BuildKitDevelopmentExecutor,type BuildKitExecutorConfig } from "./development-build-executor.js";
import { DevelopmentControllerRuntime } from "./development-controller-runtime.js";
import { PgDevelopmentControllerStore } from "./development-controller-store.js";

export async function startDevelopmentController(signal:AbortSignal):Promise<void>{
  const config=await loadDevelopmentControllerConfig(),database=createDatabase(config.databaseUrl);
  const onError=(error:Error)=>process.stderr.write(`Development controller database pool error name=DatabasePoolError code=${databaseCode(error)}\n`);database.on("error",onError);
  try{
    await database.query("SELECT 1");
    const executor=new BuildKitDevelopmentExecutor(config.buildKit),store=new PgDevelopmentControllerStore(database);
    const runtime=new DevelopmentControllerRuntime(store,executor,config.runtime,event=>process.stdout.write(`${JSON.stringify(event)}\n`));
    await runtime.run(signal);
  }finally{database.off("error",onError);await database.end();}
}

interface MainConfig {databaseUrl:string;buildKit:BuildKitExecutorConfig;runtime:{workerId:string;leaseSeconds:number;retryDelaySeconds:number;pollMilliseconds:number}}
export async function loadDevelopmentControllerConfig():Promise<MainConfig>{
  const root=process.env.BLAZN_DEVELOPMENT_SECRETS_ROOT??"/etc/blazn/development-controller/secrets";
  if(!root.startsWith("/"))throw new Error("BLAZN_DEVELOPMENT_SECRETS_ROOT must be absolute");
  const read=async(name:string)=>{const value=(await readFile(`${root}/${name}`,"utf8")).trim();if(!value)throw new Error(`Development controller secret ${name} is empty`);return value;};
  const workerId=process.env.BLAZN_DEVELOPMENT_WORKER_ID??"development-controller-1";
  if(!/^[a-z0-9](?:[a-z0-9._-]{0,126}[a-z0-9])?$/.test(workerId))throw new Error("BLAZN_DEVELOPMENT_WORKER_ID is invalid");
  const evidenceCommand=process.env.BLAZN_DEVELOPMENT_EVIDENCE_COMMAND??"/usr/local/libexec/blazn-development-evidence";
  const buildctlPath=process.env.BLAZN_BUILDCTL_PATH??"/usr/local/bin/buildctl";
  const integer=(name:string,fallback:number,min:number,max:number)=>{const raw=process.env[name];if(raw===undefined)return fallback;if(!/^[0-9]+$/.test(raw))throw new Error(`${name} is invalid`);const value=Number(raw);if(!Number.isSafeInteger(value)||value<min||value>max)throw new Error(`${name} is invalid`);return value;};
  return{
    databaseUrl:await read("database-url"),
    buildKit:{buildctlPath,address:await read("buildkit-address"),serverName:await read("buildkit-server-name"),builderId:await read("buildkit-builder-id"),registryAuthority:await read("registry-authority"),caPath:`${root}/buildkit-ca.pem`,certificatePath:`${root}/buildkit-cert.pem`,keyPath:`${root}/buildkit-key.pem`,evidenceCommand,evidenceSecretsRoot:root,maximumArtifactBytes:integer("BLAZN_DEVELOPMENT_ARTIFACT_BYTES",1024*1024,1024,2*1024*1024),maximumTotalArtifactBytes:integer("BLAZN_DEVELOPMENT_TOTAL_ARTIFACT_BYTES",3*1024*1024,1024,3*1024*1024),executionTimeoutSeconds:integer("BLAZN_DEVELOPMENT_EXECUTION_TIMEOUT_SECONDS",3600,60,7200)},
    runtime:{workerId,leaseSeconds:integer("BLAZN_DEVELOPMENT_LEASE_SECONDS",120,10,300),retryDelaySeconds:integer("BLAZN_DEVELOPMENT_RETRY_DELAY_SECONDS",15,0,300),pollMilliseconds:integer("BLAZN_DEVELOPMENT_POLL_MILLISECONDS",1000,100,60000)},
  };
}
function databaseCode(error:Error){return"code" in error&&typeof error.code==="string"&&/^[A-Z0-9]{5}$/.test(error.code)?error.code:"unknown";}

if(import.meta.url===`file://${process.argv[1]}`){const controller=new AbortController();for(const event of ["SIGINT","SIGTERM"] as const)process.once(event,()=>controller.abort(new Error(event)));startDevelopmentController(controller.signal).catch(error=>{if(!controller.signal.aborted){process.stderr.write(`${error instanceof Error?error.message:"Development controller failed"}\n`);process.exitCode=1;}});}

import { readFile } from "node:fs/promises";
import { createDatabase } from "./db.js";
import { DevelopmentSandboxEvidenceOrchestrator,PgDevelopmentSandboxEvidenceStore,type DevelopmentSandboxCommandTransport,type DevelopmentSandboxEvidenceInput } from "./development-sandbox-evidence.js";

export async function runDevelopmentEvidenceCollector(input:Buffer,signal:AbortSignal):Promise<never>{
  if(input.byteLength<2||input.byteLength>4*1024*1024)throw new Error("Development evidence collector input is invalid");
  let request:unknown;try{request=JSON.parse(input.toString("utf8"));}catch{throw new Error("Development evidence collector input is invalid JSON");}
  if(!request||typeof request!=="object"||Array.isArray(request))throw new Error("Development evidence collector input is invalid");
  const root=process.env.BLAZN_DEVELOPMENT_SECRETS_ROOT??"/etc/blazn/development-controller/secrets";
  if(!root.startsWith("/")||root.includes("\0"))throw new Error("Development evidence collector secrets root is invalid");
  const databaseUrl=(await readFile(`${root}/database-url`,"utf8")).trim();
  if(!databaseUrl)throw new Error("Development evidence collector database configuration is empty");
  const database=createDatabase(databaseUrl),unavailable:DevelopmentSandboxCommandTransport={execute:async()=>{throw new Error("Development candidate Sandbox command transport is not installed");}};
  try{return await new DevelopmentSandboxEvidenceOrchestrator(new PgDevelopmentSandboxEvidenceStore(database),unavailable).execute(request as DevelopmentSandboxEvidenceInput,signal);}
  finally{await database.end();}
}

async function readInput(){const chunks:Buffer[]=[];let size=0;for await(const value of process.stdin){const chunk=Buffer.from(value as Uint8Array);size+=chunk.byteLength;if(size>4*1024*1024)throw new Error("Development evidence collector input is too large");chunks.push(chunk);}return Buffer.concat(chunks);}
export async function mainDevelopmentEvidenceCollector(){const controller=new AbortController();for(const event of ["SIGINT","SIGTERM"] as const)process.once(event,()=>controller.abort(new Error(event)));try{await runDevelopmentEvidenceCollector(await readInput(),controller.signal);}catch(error){process.stderr.write(`${error instanceof Error?error.message:"Development evidence collector failed"}\n`);process.exitCode=1;}}
if(import.meta.url===`file://${process.argv[1]}`)await mainDevelopmentEvidenceCollector();

import { readFile } from "node:fs/promises";
import type { Server } from "node:http";
import { createDatabase } from "./db.js";
import { readJoinCredentialKey } from "./node-broker-crypto.js";
import { createNodeBrokerServer } from "./node-broker-http.js";
import { NodeBrokerService } from "./node-broker-service.js";
import { PgNodeBrokerStore } from "./node-broker-store.js";
import type { WorkerCredentialIssuer } from "./node-broker-types.js";

export async function startNodeBroker(issuer?:WorkerCredentialIssuer):Promise<Server>{if(!issuer)throw new Error("Node broker startup requires an injected worker-only MicroK8s credential issuer");const root=process.env.BLAZN_NODE_BROKER_SECRETS_ROOT??"/etc/blazn/node-broker/secrets",databaseUrl=(await readFile(`${root}/database-url`,"utf8")).trim();if(!databaseUrl)throw new Error("Node broker database URL is empty");const port=Number(process.env.NODE_BROKER_PORT??"8081");if(!Number.isSafeInteger(port)||port<1||port>65535)throw new Error("Node broker port is invalid");const database=createDatabase(databaseUrl),service=new NodeBrokerService(new PgNodeBrokerStore(database),()=>readJoinCredentialKey(`${root}/join-credential-v1`),issuer),server=createNodeBrokerServer(service);await new Promise<void>((resolve,reject)=>{server.once("error",reject);server.listen(port,"127.0.0.1",resolve);});server.once("close",()=>void database.end());return server;}

if(import.meta.url===`file://${process.argv[1]}`)startNodeBroker().catch((error:unknown)=>{process.stderr.write(`${error instanceof Error?error.message:"Node broker startup failed"}\n`);process.exitCode=1;});

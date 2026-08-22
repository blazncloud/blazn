import { readFile } from "node:fs/promises";
import type { Server } from "node:http";
import { createDatabase } from "./db.js";
import { readJoinCredentialKey } from "./node-broker-crypto.js";
import { createNodeBrokerServer } from "./node-broker-http.js";
import { NodeBrokerService } from "./node-broker-service.js";
import { PgNodeBrokerStore } from "./node-broker-store.js";
import type { WorkerCredentialIssuer } from "./node-broker-types.js";
import { defaultMicroK8sIssuerSocket, UnixMicroK8sWorkerCredentialIssuer } from "./microk8s-worker-issuer.js";

export async function startNodeBroker(issuer?: WorkerCredentialIssuer): Promise<Server> {
  const resolvedIssuer = issuer ?? await UnixMicroK8sWorkerCredentialIssuer.connect(defaultMicroK8sIssuerSocket);
  if (!resolvedIssuer.health) throw new Error("Node broker issuer health protocol is unavailable");
  await resolvedIssuer.health(AbortSignal.timeout(5_000));
  const root = process.env.BLAZN_NODE_BROKER_SECRETS_ROOT ?? "/etc/blazn/node-broker/secrets";
  const databaseUrl = (await readFile(`${root}/database-url`, "utf8")).trim();
  if (!databaseUrl) throw new Error("Node broker database URL is empty");
  const port = Number(process.env.NODE_BROKER_PORT ?? "8081");
  if (!Number.isSafeInteger(port) || port < 1 || port > 65535) throw new Error("Node broker port is invalid");
  const database = createDatabase(databaseUrl);
  try {
    await database.query({ text: "SELECT 1", query_timeout: 1_500 });
    const startupKey = await readJoinCredentialKey(`${root}/join-credential-v1`);
    startupKey.fill(0);
    const service = new NodeBrokerService(new PgNodeBrokerStore(database), () => readJoinCredentialKey(`${root}/join-credential-v1`), resolvedIssuer);
    const server = createNodeBrokerServer(service);
    await new Promise<void>((resolve, reject) => { server.once("error", reject); server.listen(port, "127.0.0.1", resolve); });
    server.once("close", () => void database.end());
    return server;
  } catch (error) {
    await database.end();
    throw error;
  }
}

if (import.meta.url === `file://${process.argv[1]}`) startNodeBroker().catch((error: unknown) => { process.stderr.write(`${error instanceof Error ? error.message : "Node broker startup failed"}\n`); process.exitCode = 1; });

import { request as httpRequest } from "node:http";
import { lstat } from "node:fs/promises";
import type {
  IssuedWorkerCredential,
  WorkerCredentialIssueRequest,
  WorkerCredentialIssuer,
} from "./node-broker-types.js";

const schemaVersion = "blazn.dev/microk8s-worker-issuer/v1";
const maxResponseBytes = 16 * 1024;

function exactKeys(value: Record<string, unknown>, keys: string[]): void {
  const actual = Object.keys(value).sort();
  const expected = [...keys].sort();
  if (actual.length !== expected.length || actual.some((key, index) => key !== expected[index])) {
    throw new Error("MicroK8s worker issuer returned an invalid response");
  }
}

function object(value: unknown): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("MicroK8s worker issuer returned an invalid response");
  }
  return value as Record<string, unknown>;
}

function uuid(value: unknown): value is string {
  return typeof value === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(value);
}

export class UnixMicroK8sWorkerCredentialIssuer implements WorkerCredentialIssuer {
  constructor(private readonly socketPath: string, private readonly timeoutMs = 5_000) {
    if (!socketPath.startsWith("/") || timeoutMs < 1 || timeoutMs > 30_000) {
      throw new Error("MicroK8s worker issuer configuration is invalid");
    }
  }

  static async connect(socketPath: string, timeoutMs = 5_000): Promise<UnixMicroK8sWorkerCredentialIssuer> {
    const info = await lstat(socketPath);
    if (!info.isSocket() || info.isSymbolicLink() || info.nlink !== 1 || info.uid !== 0 || (info.mode & 0o007) !== 0) {
      throw new Error("MicroK8s worker issuer socket is unsafe");
    }
    return new UnixMicroK8sWorkerCredentialIssuer(socketPath, timeoutMs);
  }

  async issue(request: WorkerCredentialIssueRequest, signal: AbortSignal): Promise<IssuedWorkerCredential> {
    const response = object(await this.call({ schemaVersion, operation: "issue", ...request }, signal));
    exactKeys(response, ["schemaVersion", "operation", "providerHandle", "credential", "clusterId", "clusterHealthy", "workerOnly", "expiresAt"]);
    const expiresAt = new Date(typeof response.expiresAt === "string" ? response.expiresAt : "");
    if (response.schemaVersion !== schemaVersion || response.operation !== "issue" ||
        !uuid(response.providerHandle) || typeof response.credential !== "string" ||
        !/^[A-Za-z0-9_-]{43,4096}$/.test(response.credential) ||
        response.clusterId !== request.clusterId || response.clusterHealthy !== true ||
        response.workerOnly !== true || Number.isNaN(expiresAt.getTime())) {
      throw new Error("MicroK8s worker issuer returned an invalid response");
    }
    return { providerHandle: response.providerHandle, credential: response.credential, clusterId: request.clusterId, clusterHealthy: true, workerOnly: true, expiresAt };
  }

  async revoke(providerHandle: string, signal: AbortSignal): Promise<void> {
    const response = object(await this.call({ schemaVersion, operation: "revoke", providerHandle }, signal));
    exactKeys(response, ["schemaVersion", "operation", "providerHandle", "revoked"]);
    if (response.schemaVersion !== schemaVersion || response.operation !== "revoke" || response.providerHandle !== providerHandle || response.revoked !== true) {
      throw new Error("MicroK8s worker issuer returned an invalid response");
    }
  }

  private call(body: Record<string, unknown>, signal: AbortSignal): Promise<unknown> {
    const payload = Buffer.from(JSON.stringify(body));
    return new Promise((resolve, reject) => {
      const req = httpRequest({ socketPath: this.socketPath, path: "/v1/worker-credentials", method: "POST", signal,
        headers: { "content-type": "application/json", "content-length": payload.length } }, (res) => {
        const chunks: Buffer[] = [];
        let size = 0;
        res.on("data", (chunk: Buffer) => {
          size += chunk.length;
          if (size > maxResponseBytes) req.destroy(new Error("MicroK8s worker issuer response is too large"));
          else chunks.push(chunk);
        });
        res.on("end", () => {
          try {
            const parsed: unknown = JSON.parse(Buffer.concat(chunks).toString("utf8"));
            if (res.statusCode !== 200) {
              const error = object(parsed);
              throw new Error(typeof error.message === "string" ? error.message : "MicroK8s worker issuer request failed");
            }
            resolve(parsed);
          } catch (error) { reject(error); }
        });
      });
      req.setTimeout(this.timeoutMs, () => req.destroy(new Error("MicroK8s worker issuer deadline exceeded")));
      req.once("error", reject);
      req.end(payload);
    });
  }
}

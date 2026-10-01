import { createHash, randomBytes, randomUUID } from "node:crypto";
import type { AgentRunExecutionStore, AgentRunGrantKind, AgentRunLease } from "./agent-run-execution-store.js";

export class SandboxAccessError extends Error {
  constructor(readonly code: "grant_denied" | "access_failed" | "access_timeout", message: string) { super(message); }
}

export interface SandboxExecResult { exitCode: number; stdout: Buffer; stderr: Buffer; truncated: boolean }

/** What the Agent Run executor needs from the Run's Sandbox. */
export interface SandboxAccess {
  exec(lease: AgentRunLease, command: string[], timeoutMs?: number): Promise<SandboxExecResult>;
  upload(lease: AgentRunLease, path: string, content: Buffer): Promise<void>;
  download(lease: AgentRunLease, path: string): Promise<Buffer>;
}

const maxFileBytes = 8 * 1024 * 1024;

/**
 * Reaches the Sandbox through the Sandbox controller's access endpoint. Each
 * call mints a single-use grant bound to the Run; the controller consumes it
 * before touching the Sandbox, so access ends with the Run or the lease.
 */
export class GrantSandboxAccess implements SandboxAccess {
  private readonly base: string;
  constructor(private readonly store: AgentRunExecutionStore, accessUrl: string, private readonly fetcher: typeof fetch = fetch) {
    const url = new URL(accessUrl);
    if (url.protocol !== "http:" || url.pathname !== "/" || url.search || url.hash || url.username || url.password) throw new Error("Sandbox access URL must be a plain http origin");
    this.base = url.origin;
  }

  async exec(lease: AgentRunLease, command: string[], timeoutMs = 55_000): Promise<SandboxExecResult> {
    if (command.length < 1 || command.length > 32 || command.some((part) => !part || Buffer.byteLength(part) > 1024)) throw new Error("Sandbox command is invalid");
    const response = await this.call(lease, "exec", "exec", { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ command }) }, timeoutMs);
    const body = await response.json() as { remoteExitCode?: unknown; stdoutBase64?: unknown; stderrBase64?: unknown; truncated?: unknown };
    if (!Number.isSafeInteger(body.remoteExitCode) || typeof body.stdoutBase64 !== "string" || typeof body.stderrBase64 !== "string") throw new SandboxAccessError("access_failed", "Sandbox exec response is invalid");
    return { exitCode: body.remoteExitCode as number, stdout: Buffer.from(body.stdoutBase64, "base64"), stderr: Buffer.from(body.stderrBase64, "base64"), truncated: body.truncated === true };
  }

  async upload(lease: AgentRunLease, path: string, content: Buffer): Promise<void> {
    if (content.length > maxFileBytes) throw new Error("Sandbox upload is too large");
    const digest = `sha256:${createHash("sha256").update(content).digest("hex")}`;
    await this.call(lease, "upload", "file", { method: "PUT", headers: { "content-type": "application/octet-stream", "x-blazn-sandbox-path": path, "x-content-size": String(content.length), "x-content-sha256": digest }, body: new Uint8Array(content) }, 55_000);
  }

  async download(lease: AgentRunLease, path: string): Promise<Buffer> {
    const response = await this.call(lease, "download", "file", { method: "GET", headers: { "x-blazn-sandbox-path": path } }, 55_000);
    const content = Buffer.from(await response.arrayBuffer());
    const digest = `sha256:${createHash("sha256").update(content).digest("hex")}`;
    if (content.length > maxFileBytes || response.headers.get("x-content-sha256") !== digest) throw new SandboxAccessError("access_failed", "Sandbox download failed its integrity check");
    return content;
  }

  private async call(lease: AgentRunLease, kind: AgentRunGrantKind, suffix: "exec" | "file", init: RequestInit, timeoutMs: number): Promise<Response> {
    const grantId = randomUUID(), token = randomBytes(32).toString("base64url");
    const issued = await this.store.issueGrant(lease, grantId, createHash("sha256").update(token).digest("hex"), kind, 60);
    if (!issued) throw new SandboxAccessError("grant_denied", "the Run no longer authorizes Sandbox access");
    let response: Response;
    try {
      response = await this.fetcher(`${this.base}/internal/v1/sandbox-access-grants/${grantId}/${suffix}`, {
        ...init, headers: { ...(init.headers as Record<string, string>), authorization: `Blazn-Grant ${token}` }, redirect: "error", signal: AbortSignal.timeout(timeoutMs),
      });
    } catch (error) {
      const timedOut = error instanceof Error && (error.name === "TimeoutError" || error.name === "AbortError");
      throw new SandboxAccessError(timedOut ? "access_timeout" : "access_failed", timedOut ? "Sandbox access timed out" : "Sandbox access endpoint is unreachable");
    }
    if (!response.ok) {
      let code = "";
      try { code = String(((await response.json()) as { code?: unknown }).code ?? ""); } catch { /* not JSON */ }
      throw new SandboxAccessError(response.status === 410 ? "grant_denied" : "access_failed", `Sandbox ${kind} failed with HTTP ${response.status}${/^[a-z_]{1,64}$/.test(code) ? ` (${code})` : ""}`);
    }
    return response;
  }
}

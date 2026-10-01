import { readFile } from "node:fs/promises";

/**
 * A model route the Agent Run controller may send a Run's model requests to.
 * The credential stays in the controller: the harness in the Sandbox only
 * sees the request it wrote and the response it gets back.
 */
export interface ModelRoute {
  id: string;
  version: number;
  kind: "openai-chat" | "stub";
  model: string;
  baseUrl?: string;
  credentialFile?: string;
  timeoutSeconds: number;
  maxOutputTokens?: number;
}

export interface ModelUsage { promptTokens?: number; completionTokens?: number }
export type ModelOutcome =
  | { ok: true; response: { choices: unknown[] }; usage: ModelUsage; durationMs: number }
  | { ok: false; error: { code: string; message: string }; durationMs: number };

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const maxRequestBytes = 7 * 1024 * 1024, maxResponseBytes = 4 * 1024 * 1024;

export function parseModelRoutes(text: string): ModelRoute[] {
  const parsed = JSON.parse(text) as { routes?: unknown };
  if (!parsed || !Array.isArray(parsed.routes) || parsed.routes.length > 64) throw new Error("model routes file must contain a routes array");
  const seen = new Set<string>();
  return parsed.routes.map((entry) => {
    const value = entry as Record<string, unknown>;
    const allowed = ["id", "version", "kind", "model", "baseUrl", "credentialFile", "timeoutSeconds", "maxOutputTokens"];
    if (!value || typeof value !== "object" || Object.keys(value).some((key) => !allowed.includes(key))) throw new Error("model route has unknown fields");
    if (typeof value.id !== "string" || !UUID.test(value.id) || !Number.isSafeInteger(value.version) || (value.version as number) < 1) throw new Error("model route identity is invalid");
    if (value.kind !== "openai-chat" && value.kind !== "stub") throw new Error("model route kind is unsupported");
    if (typeof value.model !== "string" || !/^[A-Za-z0-9][A-Za-z0-9._:\/-]{0,127}$/.test(value.model)) throw new Error("model route model is invalid");
    const key = `${value.id}:${value.version}`;
    if (seen.has(key)) throw new Error("model route is duplicated");
    seen.add(key);
    const timeoutSeconds = value.timeoutSeconds === undefined ? 300 : value.timeoutSeconds;
    if (!Number.isSafeInteger(timeoutSeconds) || (timeoutSeconds as number) < 5 || (timeoutSeconds as number) > 1800) throw new Error("model route timeout is invalid");
    const route: ModelRoute = { id: value.id, version: value.version as number, kind: value.kind, model: value.model, timeoutSeconds: timeoutSeconds as number };
    if (value.maxOutputTokens !== undefined) {
      if (!Number.isSafeInteger(value.maxOutputTokens) || (value.maxOutputTokens as number) < 1 || (value.maxOutputTokens as number) > 1_000_000) throw new Error("model route maxOutputTokens is invalid");
      route.maxOutputTokens = value.maxOutputTokens as number;
    }
    if (route.kind === "openai-chat") {
      if (typeof value.baseUrl !== "string") throw new Error("openai-chat route needs a baseUrl");
      const url = new URL(value.baseUrl);
      if ((url.protocol !== "https:" && url.protocol !== "http:") || url.search || url.hash || url.username || url.password) throw new Error("model route baseUrl is invalid");
      route.baseUrl = url.href.replace(/\/$/, "");
      if (value.credentialFile !== undefined) {
        if (typeof value.credentialFile !== "string" || !value.credentialFile.startsWith("/")) throw new Error("model route credentialFile must be an absolute path");
        route.credentialFile = value.credentialFile;
      }
    } else if (value.baseUrl !== undefined || value.credentialFile !== undefined) throw new Error("a stub route takes no endpoint or credential");
    return route;
  });
}

export class ModelRouter {
  private readonly routes = new Map<string, ModelRoute>();
  constructor(routes: ModelRoute[], private readonly fetcher: typeof fetch = fetch, private readonly readCredential: (path: string) => Promise<string> = async (path) => (await readFile(path, "utf8")).trim()) {
    for (const route of routes) this.routes.set(`${route.id}:${route.version}`, route);
  }

  route(id: string, version: number): ModelRoute | undefined { return this.routes.get(`${id}:${version}`); }

  /** Sends one chat request from the harness to the route and returns only the fields the harness needs. */
  async complete(route: ModelRoute, request: unknown, signal: AbortSignal): Promise<ModelOutcome> {
    const started = Date.now(), failed = (code: string, message: string): ModelOutcome => ({ ok: false, error: { code, message }, durationMs: Date.now() - started });
    const body = request as { messages?: unknown; tools?: unknown; tool_choice?: unknown; max_tokens?: unknown };
    if (!body || typeof body !== "object" || !Array.isArray(body.messages) || body.messages.length < 1 || body.messages.length > 4096) return failed("model_request_invalid", "the harness model request is invalid");
    if (route.kind === "stub") return { ok: true, response: stubResponse(body.messages as StubMessage[]), usage: {}, durationMs: Date.now() - started };
    const outbound: Record<string, unknown> = { model: route.model, messages: body.messages, stream: false };
    if (Array.isArray(body.tools) && body.tools.length > 0) { outbound.tools = body.tools; outbound.tool_choice = body.tool_choice === "none" ? "none" : "auto"; }
    const limit = route.maxOutputTokens ?? (Number.isSafeInteger(body.max_tokens) && (body.max_tokens as number) > 0 ? body.max_tokens as number : undefined);
    if (limit !== undefined) outbound.max_tokens = limit;
    const encoded = JSON.stringify(outbound);
    if (Buffer.byteLength(encoded) > maxRequestBytes) return failed("model_request_too_large", "the model request is too large");
    const headers: Record<string, string> = { "content-type": "application/json", accept: "application/json" };
    if (route.credentialFile) {
      let credential: string;
      try { credential = await this.readCredential(route.credentialFile); } catch { return failed("model_credential_unavailable", "the model route credential is unavailable"); }
      if (!credential || /\s/.test(credential)) return failed("model_credential_unavailable", "the model route credential is unavailable");
      headers.authorization = `Bearer ${credential}`;
    }
    for (let attempt = 1; ; attempt++) {
      let response: Response;
      try {
        response = await this.fetcher(`${route.baseUrl}/chat/completions`, { method: "POST", headers, body: encoded, redirect: "error", signal: AbortSignal.any([signal, AbortSignal.timeout(route.timeoutSeconds * 1000)]) });
      } catch (error) {
        if (signal.aborted) return failed("model_cancelled", "the model request was cancelled");
        const timedOut = error instanceof Error && error.name === "TimeoutError";
        if (!timedOut && attempt === 1) { await pause(2000, signal); continue; }
        return failed(timedOut ? "model_timeout" : "model_unreachable", timedOut ? "the model did not answer in time" : "the model endpoint is unreachable");
      }
      if (response.status >= 500 && attempt === 1) { await response.body?.cancel().catch(() => undefined); await pause(2000, signal); continue; }
      if (!response.ok) {
        await response.body?.cancel().catch(() => undefined);
        const code = response.status === 429 ? "model_rate_limited" : response.status === 401 || response.status === 403 ? "model_credential_rejected" : response.status >= 500 ? "model_unavailable" : "model_rejected";
        return failed(code, `the model endpoint answered HTTP ${response.status}`);
      }
      const text = await response.text();
      if (Buffer.byteLength(text) > maxResponseBytes) return failed("model_response_too_large", "the model response is too large");
      let parsed: { choices?: unknown; usage?: { prompt_tokens?: unknown; completion_tokens?: unknown } };
      try { parsed = JSON.parse(text); } catch { return failed("model_response_invalid", "the model response is not JSON"); }
      if (!Array.isArray(parsed.choices) || parsed.choices.length === 0) return failed("model_response_invalid", "the model response has no choices");
      const usage: ModelUsage = {};
      if (Number.isSafeInteger(parsed.usage?.prompt_tokens)) usage.promptTokens = parsed.usage!.prompt_tokens as number;
      if (Number.isSafeInteger(parsed.usage?.completion_tokens)) usage.completionTokens = parsed.usage!.completion_tokens as number;
      return { ok: true, response: { choices: [slimChoice(parsed.choices[0])] }, usage, durationMs: Date.now() - started };
    }
  }
}

/** Keeps the assistant text and tool calls; drops provider-specific extras. */
function slimChoice(choice: unknown) {
  const message = ((choice ?? {}) as { message?: Record<string, unknown> }).message ?? {};
  const slim: Record<string, unknown> = { role: "assistant", content: typeof message.content === "string" ? message.content : null };
  if (Array.isArray(message.tool_calls) && message.tool_calls.length > 0) slim.tool_calls = message.tool_calls;
  return { message: slim };
}

interface StubMessage { role?: string; content?: unknown }

/**
 * A deterministic stand-in model for qualification. A user message makes it
 * run one command in the Sandbox; the command's output makes it reply.
 */
function stubResponse(messages: StubMessage[]): { choices: unknown[] } {
  const last = messages[messages.length - 1] ?? {};
  const text = typeof last.content === "string" ? last.content : "";
  if (last.role === "tool") {
    const firstLine = text.split("\n").find((line) => line.trim() !== "") ?? "";
    return { choices: [{ message: { role: "assistant", content: `Stub model: the sandbox command printed "${firstLine.slice(0, 200)}".` } }] };
  }
  const echoed = text.replace(/[^A-Za-z0-9 .,_-]/g, " ").slice(0, 80);
  const command = `printf 'blazn stub model saw: %s\\n' '${echoed}' > BLAZN_AGENT_PROOF.md && cat BLAZN_AGENT_PROOF.md`;
  return { choices: [{ message: { role: "assistant", content: null, tool_calls: [{ id: `stub-${messages.length}`, type: "function", function: { name: "run_command", arguments: JSON.stringify({ command }) } }] } }] };
}

function pause(milliseconds: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const timer = setTimeout(done, milliseconds);
    function done() { clearTimeout(timer); signal.removeEventListener("abort", done); resolve(); }
    signal.addEventListener("abort", done, { once: true });
  });
}

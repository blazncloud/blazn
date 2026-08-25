import { createServer } from "node:http";
import { assertNoActiveIdentityProviders } from "./assert-no-active-idps.mjs";

const port = Number(process.env.BLAZN_PROVIDER_GATE_PORT ?? "3100");
if (!Number.isInteger(port) || port < 1024 || port > 65_535) throw new Error("provider gate port is invalid");
const server = createServer(async (request, response) => {
  const path = new URL(request.url ?? "/", "http://gate.invalid").pathname;
  if (path !== "/authorize" && path !== "/healthz") {
    response.writeHead(404, { "cache-control": "no-store", "content-type": "text/plain; charset=utf-8" });
    response.end("not found\n");
    return;
  }
  try {
    await assertNoActiveIdentityProviders();
    response.writeHead(path === "/authorize" ? 204 : 200, { "cache-control": "no-store" });
    response.end();
  } catch (error) {
    console.error(error instanceof Error ? error.message : "external identity provider safe-off gate failed");
    response.writeHead(503, { "cache-control": "no-store", "content-type": "text/plain; charset=utf-8" });
    response.end("identity login is temporarily unavailable\n");
  }
});
server.headersTimeout = 10_000;
server.requestTimeout = 10_000;
server.keepAliveTimeout = 5_000;
server.listen(port, "0.0.0.0");

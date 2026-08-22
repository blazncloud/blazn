import type { IncomingMessage, ServerResponse } from "node:http";
import type { SandboxHttpRouter } from "./sandbox-http.js";
import type { SandboxPrincipal } from "./sandbox-types.js";

export interface AuthenticatedSandboxSession {
  sessionId: string;
  userId: string;
  email: string;
  displayName: string;
}

export async function routeSandboxRequest(
  router: SandboxHttpRouter,
  request: IncomingMessage,
  response: ServerResponse,
  url: URL,
  authenticate: () => Promise<AuthenticatedSandboxSession>,
): Promise<boolean> {
  if (!router.matches(url.pathname)) return false;
  await router.handle(request, response, url, async (): Promise<SandboxPrincipal> => {
    const session = await authenticate();
    return {
      sessionId: session.sessionId,
      userId: session.userId,
      email: session.email,
      displayName: session.displayName,
    };
  });
  return true;
}

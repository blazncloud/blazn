import type { ServerResponse } from "node:http";
import { renderActivationErrorPage, renderActivationLandingPage, renderActivationPage, sendHtml, type AuthMode } from "./auth-page.js";

const CODE_ALPHABET = /^[A-HJ-NP-Z2-9\s-]+$/i;
const CODE_VALUE = /^[A-HJ-NP-Z2-9]{8}$/;

export interface PendingActivation {
  id: string;
  deviceName: string;
  platform: string;
  publicKey: string;
}

export interface ActivationHttpDependencies {
  lookup(code: string): Promise<PendingActivation | undefined>;
  oidcEnabled: boolean;
  publicKeyDigest(publicKey: string): string;
  activationConfirmation?(input: { authorizationId: string; userCode: string; mode: AuthMode; publicKeyDigest: string }): string;
}

export function normalizeActivationCode(value: string): string | undefined {
  const candidate = value.trim();
  if (!candidate || !CODE_ALPHABET.test(candidate)) return undefined;
  const compact = candidate.toUpperCase().replace(/[\s-]/g, "");
  if (!CODE_VALUE.test(compact)) return undefined;
  return `${compact.slice(0, 4)}-${compact.slice(4)}`;
}

export async function serveActivationPage(response: ServerResponse, url: URL, dependencies: ActivationHttpDependencies): Promise<void> {
  const suppliedCode = url.searchParams.get("user_code");
  if (suppliedCode === null || suppliedCode.trim() === "") {
    sendHtml(response, 200, renderActivationLandingPage());
    return;
  }
  const code = normalizeActivationCode(suppliedCode);
  if (!code) {
    sendHtml(response, 400, renderActivationErrorPage("Enter the eight-letter code shown by your CLI."));
    return;
  }
  const authorization = await dependencies.lookup(code);
  if (!authorization) {
    sendHtml(response, 404, renderActivationErrorPage("The code is invalid, expired, or has already been used."));
    return;
  }
  const mode: AuthMode = url.searchParams.get("mode") === "signup" ? "signup" : "signin";
  const publicKeyDigest = dependencies.publicKeyDigest(authorization.publicKey);
  const activationConfirmation = dependencies.oidcEnabled
    ? dependencies.activationConfirmation?.({ authorizationId: authorization.id, userCode: code, mode, publicKeyDigest })
    : undefined;
  sendHtml(response, 200, renderActivationPage({ code, deviceName: authorization.deviceName, platform: authorization.platform, mode, oidcEnabled: dependencies.oidcEnabled, publicKeyDigest, ...(activationConfirmation ? { activationConfirmation } : {}) }));
}

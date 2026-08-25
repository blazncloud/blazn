import type { ServerResponse } from "node:http";

export type AuthMode = "signin" | "signup";

export interface ActivationPageInput {
  code: string;
  deviceName: string;
  platform: string;
  mode: AuthMode;
  oidcEnabled: boolean;
  activationConfirmation?: string;
  publicKeyDigest: string;
}

export function escapeHtml(value: string): string {
  return value.replace(/[&<>"']/g, (character) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[character] ?? character);
}

function flame(): string {
  return `<svg aria-hidden="true" viewBox="0 0 1024 1024"><rect x="64" y="64" width="896" height="896" rx="200" fill="#101010"/><rect x="64" y="64" width="896" height="896" rx="200" fill="none" stroke="rgba(255,255,255,.13)" stroke-width="8"/><svg x="294" y="212" width="436" height="600" viewBox="120 90 160 220"><g transform="translate(0 400) scale(.1 -.1)" fill="#f97316"><path d="M1892 3003c-23-20-529-827-572-913-168-334-58-750 255-963 408-278 983-83 1140 386 70 212 46 440-67 632-52 87-173 208-258 257-98 57-106 52-234-156-112-181-112-200-2-297 139-122 123-328-31-412-175-94-373 22-373 218 0 37 8 75 19 100 10 22 107 182 216 355 229 366 228 364 219 393-14 46-198 383-218 399-27 23-68 23-94 1z"/></g></svg></svg>`;
}

function identityButton(input: ActivationPageInput): string {
  if (!input.oidcEnabled) return `<div class="notice"><strong>Account creation is not enabled yet.</strong><span>The self-hosted Blazn identity service must be configured by an administrator.</span></div>`;
	if (!input.activationConfirmation) throw new Error("OIDC activation confirmation is required");
  const label = input.mode === "signup" ? "Create a secure account" : "Continue securely";
  return `<form method="post" action="/v1/auth/oidc/start"><input type="hidden" name="user_code" value="${escapeHtml(input.code)}"><input type="hidden" name="mode" value="${input.mode}"><input type="hidden" name="activation_confirmation" value="${escapeHtml(input.activationConfirmation)}"><button class="social" type="submit"><span class="provider-mark">B</span>${label}</button></form><div class="methods" aria-label="Available sign-in methods"><span>Email</span><span>Passkey</span><span>Google</span><span>GitHub</span><span>Apple</span></div>`;
}

const styles = `
:root{color-scheme:dark;font-family:Inter,ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#080808;color:#fafafa}*{box-sizing:border-box}body{margin:0;min-width:320px;min-height:100vh;background:radial-gradient(circle at 13% 8%,rgba(249,115,22,.22),transparent 32%),radial-gradient(circle at 85% 84%,rgba(194,65,12,.12),transparent 33%),#080808}.shell{min-height:100vh;display:grid;grid-template-columns:minmax(300px,1fr) minmax(440px,640px)}.story{padding:clamp(34px,6vw,88px);display:flex;flex-direction:column;justify-content:space-between;border-right:1px solid rgba(255,255,255,.08)}.brand{display:flex;align-items:center;gap:14px;font-weight:760;letter-spacing:.01em}.brand svg{width:46px;height:46px}.hero{max-width:700px}.eyebrow{color:#fb923c;font-size:12px;font-weight:800;letter-spacing:.17em;text-transform:uppercase}.hero h1{font-size:clamp(42px,6vw,78px);line-height:.98;margin:18px 0 24px;letter-spacing:-.05em}.hero p{max-width:620px;color:#b7b7bd;font-size:18px;line-height:1.7}.proof{display:flex;gap:22px;flex-wrap:wrap;color:#8d8d94;font-size:13px}.proof span:before{content:"";display:inline-block;width:6px;height:6px;margin:0 9px 2px 0;border-radius:50%;background:#f97316;box-shadow:0 0 16px #f97316}.panel{display:grid;place-items:center;padding:28px;background:rgba(15,15,15,.78);backdrop-filter:blur(22px)}.card{width:min(100%,480px);padding:34px;border:1px solid rgba(255,255,255,.1);border-radius:24px;background:linear-gradient(145deg,rgba(32,32,34,.96),rgba(15,15,16,.98));box-shadow:0 28px 90px rgba(0,0,0,.48)}.mobile-brand{display:none}.device{display:grid;grid-template-columns:1fr auto;gap:8px 16px;padding:15px 17px;margin-bottom:22px;border-radius:14px;background:#0d0d0e;border:1px solid rgba(255,255,255,.08)}.device strong{font-size:14px}.device span{color:#8e8e96;font-size:12px}.code{grid-row:1/3;grid-column:2;color:#fed7aa;font:750 12px ui-monospace,SFMono-Regular,monospace;align-self:center}.tabs{display:grid;grid-template-columns:1fr 1fr;padding:4px;margin-bottom:26px;border-radius:12px;background:#0b0b0c}.tab{padding:10px;text-align:center;border-radius:9px;color:#8e8e96;text-decoration:none;font-weight:700;font-size:13px}.tab.active{color:#fff;background:#262628;box-shadow:0 1px 4px rgba(0,0,0,.4)}h2{font-size:27px;margin:0 0 8px;letter-spacing:-.03em}.lede{color:#97979f;margin:0 0 24px;font-size:14px;line-height:1.55}.field{display:block;margin:14px 0;color:#d4d4d8;font-size:12px;font-weight:700}.field input{display:block;width:100%;margin-top:7px;padding:13px 14px;border:1px solid rgba(255,255,255,.13);border-radius:11px;background:#0c0c0d;color:white;font:inherit;outline:none}.field input:focus{border-color:#f97316;box-shadow:0 0 0 3px rgba(249,115,22,.17)}.primary,.social{display:flex;align-items:center;justify-content:center;width:100%;min-height:46px;border-radius:11px;font-weight:750;font-size:14px;text-decoration:none;cursor:pointer}.primary{border:0;color:#17100b;background:linear-gradient(135deg,#fb923c,#f97316);box-shadow:0 10px 30px rgba(249,115,22,.2)}.divider{display:flex;align-items:center;gap:12px;margin:21px 0;color:#6f6f76;font-size:11px;text-transform:uppercase;letter-spacing:.12em}.divider:before,.divider:after{content:"";height:1px;flex:1;background:rgba(255,255,255,.09)}.social{position:relative;margin:9px 0;color:#eee;border:1px solid rgba(255,255,255,.12);background:#151516}.social:hover{border-color:rgba(249,115,22,.6);background:#1c1917}.provider-mark{position:absolute;left:15px;display:grid;place-items:center;width:24px;height:24px;border-radius:7px;background:#29292c;color:#fb923c;font-weight:900}.methods{display:flex;justify-content:center;gap:8px;flex-wrap:wrap;margin-top:14px;color:#77777f;font-size:11px}.methods span{padding:4px 7px;border:1px solid rgba(255,255,255,.08);border-radius:999px}.notice{display:grid;gap:5px;padding:15px;border:1px solid rgba(251,146,60,.25);border-radius:12px;background:rgba(124,45,18,.14);font-size:13px}.notice span{color:#a8a8af;line-height:1.5}.terms{margin:20px 0 0;color:#6f6f76;font-size:11px;line-height:1.55;text-align:center}.terms a{color:#a8a8af}.success{text-align:center}.success svg{width:74px;height:74px}.success h1{margin:20px 0 10px}.success p{color:#aaaab1;line-height:1.6}@media(max-width:860px){.shell{display:block}.story{display:none}.panel{min-height:100vh;padding:18px}.mobile-brand{display:flex;justify-content:center;align-items:center;gap:10px;margin-bottom:24px;font-weight:800}.mobile-brand svg{width:40px;height:40px}.card{padding:26px 22px}}
`;

function document(title: string, body: string): string {
  return `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="theme-color" content="#101010"><title>${escapeHtml(title)}</title><style>${styles}</style></head><body>${body}</body></html>`;
}

export function renderActivationPage(input: ActivationPageInput): string {
  const signin = input.mode === "signin";
  const tabBase = `/activate?user_code=${encodeURIComponent(input.code)}`;
  const legacy = signin ? `<form method="post" action="/v1/auth/device/approve"><input type="hidden" name="user_code" value="${escapeHtml(input.code)}"><label class="field">Email<input name="email" type="email" autocomplete="username" required></label><label class="field">Password<input name="password" type="password" autocomplete="current-password" required></label><button class="primary">Authorize this device</button></form><div class="divider">or</div>` : "";
  const heading = signin ? "Welcome back" : "Create your Blazn account";
  const lede = signin ? "Sign in to approve this CLI without sharing credentials with the device." : "Create a verified identity in Blazn's self-hosted authentication service. Multi-factor authentication is required.";
  return document("Authorize Blazn", `<div class="shell"><section class="story"><div class="brand">${flame()}<span>Blazn</span></div><div class="hero"><div class="eyebrow">Your AI workforce, one command away</div><h1>Build with agents.<br>Keep control.</h1><p>Securely connect this machine to the workspace where your models, tools, environments, and team operate together.</p></div><div class="proof"><span>Device-bound sessions</span><span>Verified identities</span><span>MFA enforced</span></div></section><main class="panel"><div class="card"><div class="mobile-brand">${flame()}<span>Blazn</span></div><div class="device"><strong>${escapeHtml(input.deviceName)}</strong><span>${escapeHtml(input.platform)} · key ${escapeHtml(input.publicKeyDigest.slice(7, 19))}</span><div class="code">${escapeHtml(input.code)}</div></div><nav class="tabs" aria-label="Account access"><a class="tab ${signin ? "active" : ""}" href="${tabBase}&mode=signin">Sign in</a><a class="tab ${signin ? "" : "active"}" href="${tabBase}&mode=signup">Sign up</a></nav><h2>${heading}</h2><p class="lede">${lede}</p>${legacy}${identityButton(input)}<p class="terms">By continuing, you explicitly approve the device and public-key fingerprint shown above.</p></div></main></div>`);
}

function codeEntry(error?: string): string {
  const notice = error ? `<div class="notice" role="alert"><strong>That code could not be used.</strong><span>${escapeHtml(error)}</span></div>` : "";
  return `<form method="get" action="/activate"><label class="field">Activation code<input name="user_code" type="text" inputmode="text" autocomplete="one-time-code" autocapitalize="characters" spellcheck="false" minlength="8" maxlength="11" pattern="[A-HJ-NP-Za-hj-np-z2-9 -]{8,11}" placeholder="ABCD-EFGH" required></label><button class="primary" type="submit">Continue</button></form>${notice}`;
}

export function renderActivationLandingPage(): string {
  return document("Activate Blazn", `<div class="shell"><section class="story"><div class="brand">${flame()}<span>Blazn</span></div><div class="hero"><div class="eyebrow">Connect your CLI</div><h1>Enter the code.<br>Approve the device.</h1><p>Your CLI displays a short one-time code. Enter it here to inspect the device and choose Sign in or Sign up.</p></div><div class="proof"><span>Short-lived codes</span><span>Device-bound keys</span><span>Explicit approval</span></div></section><main class="panel"><div class="card"><div class="mobile-brand">${flame()}<span>Blazn</span></div><h2>Activate a device</h2><p class="lede">Enter the code shown by <strong>blazn auth login</strong>. Codes are short-lived and can be used only once.</p>${codeEntry()}<p class="terms">Only continue if you started this sign-in from a device you recognize.</p></div></main></div>`);
}

export function renderActivationErrorPage(message: string): string {
  return document("Activation code unavailable", `<main class="panel"><div class="card"><div class="mobile-brand">${flame()}<span>Blazn</span></div><h2>Try another activation code</h2><p class="lede">Return to your CLI if you need to request a new code.</p>${codeEntry(message)}</div></main>`);
}

export function renderAuthResult(title: string, message: string, ok: boolean): string {
  return document(title, `<main class="panel"><div class="card success">${flame()}<h1>${escapeHtml(title)}</h1><p>${escapeHtml(message)}</p>${ok ? "<p>You may close this window and return to the CLI.</p>" : "<p>Return to the activation page and try again.</p>"}</div></main>`);
}

export function sendHtml(response: ServerResponse, status: number, html: string, clearCookie = false): void {
  const headers: Record<string, string> = { "content-type": "text/html; charset=utf-8", "content-length": String(Buffer.byteLength(html)), "cache-control": "no-store", "content-security-policy": "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'", "x-content-type-options": "nosniff", "x-frame-options": "DENY", "referrer-policy": "no-referrer", "permissions-policy": "camera=(), microphone=(), geolocation=()" };
  if (clearCookie) headers["set-cookie"] = "blazn_oidc=; Path=/v1/auth/oidc/callback; Max-Age=0; HttpOnly; Secure; SameSite=Lax";
  response.writeHead(status, headers);
  response.end(html);
}

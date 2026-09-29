import assert from "node:assert/strict";
import test from "node:test";
import { renderActivationPage, renderEmailCodePage, renderOidcHandoff } from "./auth-page.js";

test("activation page is branded, escaped, and script free", () => {
  const html = renderActivationPage({ code: "ABCD-EFGH", deviceName: '<script>alert("x")</script>', platform: "darwin/arm64", mode: "signin", oidcEnabled: true, activationConfirmation: "sealed-confirmation", publicKeyDigest: `sha256:${"a".repeat(64)}` });
  assert.match(html, /Blazn/);
  assert.match(html, /#f97316/);
  assert.match(html, /Continue securely/);
  assert.match(html, /Passkey/);
  assert.match(html, /Google/);
  assert.match(html, /Account login|Email/);
  assert.doesNotMatch(html, /<script/);
  assert.doesNotMatch(html, /<script>alert/);
  assert.match(html, /&lt;script&gt;/);
	assert.match(html, /method="post" action="\/v1\/auth\/oidc\/start"/);
	assert.match(html, /name="activation_confirmation" value="sealed-confirmation"/);
	assert.doesNotMatch(html, /href="\/v1\/auth\/oidc\/start/);
});

test("signup tab routes every identity through the isolated provider", () => {
  const html = renderActivationPage({ code: "ABCD-EFGH", deviceName: "laptop", platform: "linux/amd64", mode: "signup", oidcEnabled: true, activationConfirmation: "sealed-confirmation", publicKeyDigest: `sha256:${"b".repeat(64)}` });
  assert.match(html, /Create your Blazn account/);
  assert.match(html, /mode=signup/);
  assert.match(html, /Create a secure account/);
  assert.doesNotMatch(html, /name="password"/);
});

test("signup fails visibly closed when the provider is absent", () => {
  const html = renderActivationPage({ code: "ABCD-EFGH", deviceName: "laptop", platform: "linux/amd64", mode: "signup", oidcEnabled: false, publicKeyDigest: `sha256:${"c".repeat(64)}` });
  assert.match(html, /Account creation is not enabled yet/);
  assert.doesNotMatch(html, /\/v1\/auth\/oidc\/start/);
});

test("OIDC handoff is visible, script free, and escapes the destination", () => {
  const html = renderOidcHandoff('https://auth.blazn.example/oauth/v2/authorize?state=a&label=<unsafe>');
  assert.match(html, /Continue to identity service/);
  assert.match(html, /href="https:\/\/auth\.blazn\.example\/oauth\/v2\/authorize\?state=a&amp;label=&lt;unsafe&gt;"/);
  assert.doesNotMatch(html, /<script/);
  assert.doesNotMatch(html, /http-equiv=["']refresh/i);
  assert.throws(() => renderOidcHandoff("javascript:alert(1)"), /must use HTTPS/);
});

test("email sign-in is passwordless and posts only an email", () => {
  const html = renderActivationPage({ code: "ABCD-EFGH", deviceName: "laptop", platform: "darwin/arm64", mode: "signin", oidcEnabled: false, emailEnabled: true, publicKeyDigest: `sha256:${"d".repeat(64)}` });
  assert.match(html, /method="post" action="\/v1\/auth\/device\/email-code"/);
  assert.match(html, /name="email" type="email"/);
  assert.match(html, /Continue with email/);
  assert.doesNotMatch(html, /name="password"/);
  assert.doesNotMatch(html, /Account creation is not enabled yet/);
  assert.doesNotMatch(html, /<script/);
});

test("email sign-up uses the same passwordless form", () => {
  const html = renderActivationPage({ code: "ABCD-EFGH", deviceName: "laptop", platform: "linux/amd64", mode: "signup", oidcEnabled: false, emailEnabled: true, publicKeyDigest: `sha256:${"e".repeat(64)}` });
  assert.match(html, /Create account with email/);
  assert.match(html, /name="mode" value="signup"/);
  assert.doesNotMatch(html, /name="password"/);
});

test("email code page escapes input and verifies a six-digit code", () => {
  const html = renderEmailCodePage({ code: "ABCD-EFGH", email: 'a"b@example.com', deviceName: "<laptop>", platform: "darwin/arm64", mode: "signin", error: "The code is incorrect." });
  assert.match(html, /action="\/v1\/auth\/device\/email-verify"/);
  assert.match(html, /autocomplete="one-time-code"/);
  assert.match(html, /a&quot;b@example\.com/);
  assert.match(html, /&lt;laptop&gt;/);
  assert.match(html, /The code is incorrect\./);
  assert.doesNotMatch(html, /<script/);
});

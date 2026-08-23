import assert from "node:assert/strict";
import test from "node:test";
import { renderActivationPage } from "./auth-page.js";

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

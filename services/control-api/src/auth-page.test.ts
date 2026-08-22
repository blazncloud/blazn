import assert from "node:assert/strict";
import test from "node:test";
import { renderActivationPage } from "./auth-page.js";

test("activation page is branded, escaped, and script free", () => {
  const html = renderActivationPage({ code: "ABCD-EFGH", deviceName: '<script>alert("x")</script>', platform: "darwin/arm64", mode: "signin", oidcEnabled: true, socialConnections: ["google-oauth2", "github", "apple"] });
  assert.match(html, /Blazn/);
  assert.match(html, /#f97316/);
  assert.match(html, /Continue with Google/);
  assert.match(html, /Continue with GitHub/);
  assert.match(html, /Continue with Apple/);
  assert.match(html, /Account login|Email/);
  assert.doesNotMatch(html, /<script/);
  assert.doesNotMatch(html, /<script>alert/);
  assert.match(html, /&lt;script&gt;/);
});

test("signup tab routes every identity through the isolated provider", () => {
  const html = renderActivationPage({ code: "ABCD-EFGH", deviceName: "laptop", platform: "linux/amd64", mode: "signup", oidcEnabled: true, socialConnections: ["google-oauth2"] });
  assert.match(html, /Create your Blazn account/);
  assert.match(html, /mode=signup/);
  assert.match(html, /connection=google-oauth2/);
  assert.match(html, /Create account with email/);
  assert.doesNotMatch(html, /name="password"/);
});

test("signup fails visibly closed when the provider is absent", () => {
  const html = renderActivationPage({ code: "ABCD-EFGH", deviceName: "laptop", platform: "linux/amd64", mode: "signup", oidcEnabled: false, socialConnections: [] });
  assert.match(html, /Account creation is not enabled yet/);
  assert.doesNotMatch(html, /\/v1\/auth\/oidc\/start/);
});

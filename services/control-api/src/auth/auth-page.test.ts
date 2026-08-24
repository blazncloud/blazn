import assert from "node:assert/strict";
import test from "node:test";
import { renderLegacyActivationPage, renderLegacyAuthResult } from "../auth-page.js";

test("legacy activation page is branded, escaped, responsive, and script free", () => {
  const html = renderLegacyActivationPage({ code: "ABCD-EFGH", deviceName: '<script>alert("x")</script>', platform: "linux/amd64" });
  assert.match(html, /Blazn/);
  assert.match(html, /#f97316/);
  assert.match(html, /name="user_code" value="ABCD-EFGH"/);
  assert.match(html, /@media\(max-width:860px\)/);
  assert.match(html, /&lt;script&gt;/);
  assert.doesNotMatch(html, /<script/);
});

test("legacy authorization result stays inside the branded experience", () => {
  assert.match(renderLegacyAuthResult("Device authorized", "Done", true), /return to the CLI/);
  assert.match(renderLegacyAuthResult("Authorization failed", "Nope", false), /try again/);
});

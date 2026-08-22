import assert from "node:assert/strict";
import test from "node:test";
import { invitationToken, requestDigest } from "./workspace-crypto.js";
import { roleAllows } from "./workspace-types.js";

test("fixed workspace role matrix preserves management boundaries", () => {
  assert.equal(roleAllows("owner", "manage_members"), true);
  assert.equal(roleAllows("administrator", "invite"), true);
  assert.equal(roleAllows("operator", "operate"), true);
  assert.equal(roleAllows("operator", "invite"), false);
  assert.equal(roleAllows("member", "edit"), false);
  assert.equal(roleAllows("viewer", "read"), true);
});

test("invitation token and request digests are deterministic and canonical", () => {
  const key = Buffer.alloc(32, 7);
  const first = invitationToken(key, "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", "BBBBBBBB-BBBB-4BBB-8BBB-BBBBBBBBBBBB", "idem-key");
  const second = invitationToken(key, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "idem-key");
  assert.equal(first, second);
  assert.match(first, /^[A-Za-z0-9_-]{43}$/);
  assert.equal(requestDigest({ b: 2, a: 1 }), requestDigest({ a: 1, b: 2 }));
});

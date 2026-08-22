import assert from "node:assert/strict";
import test from "node:test";
import { remoteIdentity, TrustedProxyPolicy } from "./limits.js";

function request(peer: string, forwarded?: string): Parameters<typeof remoteIdentity>[0] {
  return { socket: { remoteAddress: peer }, headers: forwarded === undefined ? {} : { "x-forwarded-for": forwarded } } as never;
}

test("trusted Docker bridge proxy separates forwarded clients", () => {
  const policy = new TrustedProxyPolicy(["172.18.0.1/32"], 1);
  assert.equal(remoteIdentity(request("172.18.0.1", "198.51.100.10"), policy), "198.51.100.10");
  assert.equal(remoteIdentity(request("172.18.0.1", "198.51.100.11"), policy), "198.51.100.11");
});

test("untrusted peers cannot spoof forwarding headers", () => {
  const policy = new TrustedProxyPolicy(["172.18.0.1/32"], 1);
  assert.equal(remoteIdentity(request("172.18.0.99", "198.51.100.10"), policy), "172.18.0.99");
});

test("forwarding chain must have the exact configured trusted hops", () => {
  const policy = new TrustedProxyPolicy(["172.18.0.0/16"], 2);
  assert.equal(remoteIdentity(request("172.18.0.1", "198.51.100.10, 172.18.0.2"), policy), "198.51.100.10");
  assert.equal(remoteIdentity(request("172.18.0.1", "198.51.100.10"), policy), "172.18.0.1");
  assert.equal(remoteIdentity(request("172.18.0.1", "192.0.2.7, 198.51.100.10, 172.18.0.2"), policy), "172.18.0.1");
  assert.equal(remoteIdentity(request("172.18.0.1", "198.51.100.10, malformed"), policy), "172.18.0.1");
  assert.equal(remoteIdentity(request("172.18.0.1", "198.51.100.10, 203.0.113.9"), policy), "172.18.0.1");
});

test("trusted proxy CIDRs fail closed", () => {
  assert.throws(() => new TrustedProxyPolicy(["172.18.0.1"], 1), /invalid trusted proxy CIDR/);
  assert.throws(() => new TrustedProxyPolicy(["172.18.0.0/99"], 1), /invalid trusted proxy CIDR/);
  assert.throws(() => new TrustedProxyPolicy([], 1), /at least one/);
});

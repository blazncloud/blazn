import assert from "node:assert/strict";
import test from "node:test";
import { loadConfig } from "./config.js";

const names = [
  "NODE_ENV", "DATABASE_URL", "S3_ENDPOINT", "S3_ACCESS_KEY", "S3_SECRET_KEY",
  "ZITADEL_ISSUER_URL", "ZITADEL_CLIENT_ID", "ZITADEL_CLIENT_SECRET",
  "OIDC_COOKIE_KEY", "ZITADEL_REQUIRE_MFA", "ZITADEL_REVIEWED_RELEASE",
  "ZITADEL_REVIEWED_ASSURANCE_POLICY_DIGEST", "ZITADEL_REVIEWED_ACR_POLICY",
  "ZITADEL_REVIEWED_MFA_AMR_SETS",
] as const;

function withIdentityEnvironment(overrides: Record<string, string>, run: () => void): void {
  const before = new Map(names.map((name) => [name, process.env[name]]));
  Object.assign(process.env, {
    NODE_ENV: "test",
    DATABASE_URL: "postgres://test",
    S3_ENDPOINT: "http://object.test",
    S3_ACCESS_KEY: "access",
    S3_SECRET_KEY: "secret",
    ZITADEL_ISSUER_URL: "https://identity.blazn.example",
    ZITADEL_CLIENT_ID: "client-id",
    ZITADEL_CLIENT_SECRET: "client-secret",
    OIDC_COOKIE_KEY: "cookie-key",
    ZITADEL_REQUIRE_MFA: "true",
    ZITADEL_REVIEWED_RELEASE: "v4.17.1",
    ZITADEL_REVIEWED_ASSURANCE_POLICY_DIGEST: `sha256:${"a".repeat(64)}`,
    ZITADEL_REVIEWED_ACR_POLICY: "zitadel-v4.17.1-empty",
    ZITADEL_REVIEWED_MFA_AMR_SETS: "pwd+mfa+otp;user+mfa",
    ...overrides,
  });
  try { run(); }
  finally {
    for (const [name, value] of before) {
      if (value === undefined) delete process.env[name];
      else process.env[name] = value;
    }
  }
}

test("ZITADEL v4.17.1 empty-ACR policy is explicit and pinned", () => {
  withIdentityEnvironment({}, () => {
    assert.deepEqual(loadConfig().zitadel?.assurancePolicy, {
      provider: "zitadel",
      reviewedRelease: "v4.17.1",
      policyDigest: `sha256:${"a".repeat(64)}`,
      acrPolicy: "zitadel-v4.17.1-empty",
      acceptedAmrSets: [["pwd", "mfa", "otp"], ["user", "mfa"]],
    });
  });
});

test("ZITADEL assurance configuration rejects release, ACR, and MFA drift", () => {
  for (const overrides of [
    { ZITADEL_REVIEWED_RELEASE: "v4.17.2" },
    { ZITADEL_REVIEWED_ACR_POLICY: "accept-empty" },
    { ZITADEL_REVIEWED_MFA_AMR_SETS: "pwd+otp;user" },
    { ZITADEL_REVIEWED_MFA_AMR_SETS: "pwd+mfa+otp" },
  ]) {
    withIdentityEnvironment(overrides, () => assert.throws(() => loadConfig(), /v4\.17\.1 empty-ACR\/MFA policy/));
  }
});

import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";

if (process.argv.length < 3 || process.argv.length > 4) throw new Error("usage: node verify-qualification.mjs RECEIPT.json [REVIEWED_ENV_FILE]");
const receipt = JSON.parse(readFileSync(process.argv[2], "utf8"));
const exact = (value, keys, label) => {
  if (!value || typeof value !== "object" || Array.isArray(value) || JSON.stringify(Object.keys(value).sort()) !== JSON.stringify([...keys].sort())) throw new Error(`${label} fields are invalid`);
};
const digest = (value, label) => { if (!/^sha256:[0-9a-f]{64}$/.test(value)) throw new Error(`${label} digest is invalid`); };
exact(receipt, ["schemaVersion", "issuer", "driverDigest", "environmentDigest", "services", "backupUtility", "gates", "identityProviders", "backup", "startedAt", "observedAt"], "receipt");
const startedMillis = Date.parse(receipt.startedAt), observedMillis = Date.parse(receipt.observedAt);
if (receipt.schemaVersion !== "blazn.identity.qualification/v3" || !String(receipt.issuer).startsWith("https://") || !Number.isFinite(startedMillis) || !Number.isFinite(observedMillis) || observedMillis < startedMillis) throw new Error("receipt identity or time range is invalid");
digest(receipt.driverDigest, "driver"); digest(receipt.environmentDigest, "environment");
const serviceNames = ["postgres", "proxy", "zitadel-api", "zitadel-login", "provider-gate-provision", "idp-gate"];
const imageRefPattern = /^[a-z0-9][a-z0-9._:/-]*@sha256:[0-9a-f]{64}$/;
exact(receipt.services, serviceNames, "services");
for (const service of serviceNames) {
  const value = receipt.services[service];
  exact(value, ["configuredImage", "before", "after"], `service ${service}`);
  if (!imageRefPattern.test(value.configuredImage)) throw new Error(`service ${service} configured image is invalid`);
  for (const phase of ["before", "after"]) {
    exact(value[phase], ["containerId", "observedConfigImage", "imageId"], `service ${service} ${phase}`);
    if (!/^[0-9a-f]{64}$/.test(value[phase].containerId) || value[phase].observedConfigImage !== value.configuredImage || !/^sha256:[0-9a-f]{64}$/.test(value[phase].imageId)) throw new Error(`service ${service} ${phase} identity mismatch`);
  }
  if (value.before.imageId !== value.after.imageId || value.before.containerId === value.after.containerId) throw new Error(`service ${service} rollback comparison failed`);
}
exact(receipt.backupUtility, ["configuredImage", "beforeImageId", "afterImageId"], "backup utility");
if (!imageRefPattern.test(receipt.backupUtility.configuredImage) || !/^sha256:[0-9a-f]{64}$/.test(receipt.backupUtility.beforeImageId) || receipt.backupUtility.beforeImageId !== receipt.backupUtility.afterImageId) throw new Error("backup utility identity or rollback comparison is invalid");
const gateNames = ["composeBootstrap", "tlsIssuer", "oidcDiscovery", "authorizationCodePkce", "verifiedEmail", "reviewedAcrMfa", "legacyLogin", "oidcAwareHealth", "deviceConfirmation"];
exact(receipt.gates, gateNames, "gates");
for (const name of gateNames) { exact(receipt.gates[name], ["evidenceDigest", "observedAt"], `gate ${name}`); digest(receipt.gates[name].evidenceDigest, `gate ${name}`); const gateTime = Date.parse(receipt.gates[name].observedAt); if (!Number.isFinite(gateTime) || gateTime < startedMillis || gateTime > observedMillis + 30_000) throw new Error(`gate ${name} time is invalid or stale`); }
exact(receipt.identityProviders, ["before", "after"], "identity providers");
for (const phase of ["before", "after"]) {
  const evidence = receipt.identityProviders[phase];
  exact(evidence, ["authorityPrincipal", "authoritySentinel", "organizationCount", "activeProviderCount", "evidenceDigest", "observedAt"], `identity providers ${phase}`);
  digest(evidence.evidenceDigest, `identity providers ${phase}`);
  const observed = Date.parse(evidence.observedAt);
  if (evidence.authorityPrincipal !== "blazn-provider-gate" || evidence.authoritySentinel !== "blazn-provider-gate-sentinel" || evidence.organizationCount !== 1 || evidence.activeProviderCount !== 0 || !Number.isFinite(observed) || observed < startedMillis || observed > observedMillis + 30_000) throw new Error(`identity providers ${phase} evidence is invalid or stale`);
}
if (Date.parse(receipt.identityProviders.after.observedAt) < Date.parse(receipt.identityProviders.before.observedAt)) throw new Error("identity provider evidence order is invalid");
const backupNames = ["manifestDigest", "databaseDigest", "masterKeyBefore", "masterKeyAfter", "patBefore", "patAfter", "preRestorePatSnapshotDigest"];
exact(receipt.backup, backupNames, "backup"); for (const name of backupNames) digest(receipt.backup[name], name);
if (receipt.backup.masterKeyBefore !== receipt.backup.masterKeyAfter || receipt.backup.patBefore !== receipt.backup.patAfter) throw new Error("backup restoration evidence mismatches");
if (process.argv[3]) {
  const raw = readFileSync(process.argv[3]);
  const environmentDigest = `sha256:${createHash("sha256").update(raw).digest("hex")}`;
  if (environmentDigest !== receipt.environmentDigest) throw new Error("reviewed environment digest mismatch");
  const environment = Object.fromEntries(raw.toString("utf8").split(/\r?\n/).filter((line) => line && !line.startsWith("#")).map((line) => { const separator = line.indexOf("="); if (separator < 1) throw new Error("reviewed environment entry is invalid"); return [line.slice(0, separator), line.slice(separator + 1)]; }));
  const expected = { postgres: environment.ZITADEL_POSTGRES_IMAGE, proxy: environment.ZITADEL_TRAEFIK_IMAGE, "zitadel-api": environment.ZITADEL_IMAGE, "zitadel-login": environment.ZITADEL_LOGIN_IMAGE, "provider-gate-provision": environment.ZITADEL_LOGIN_IMAGE, "idp-gate": environment.ZITADEL_LOGIN_IMAGE };
  for (const service of serviceNames) if (receipt.services[service].configuredImage !== expected[service]) throw new Error(`service ${service} differs from reviewed environment`);
  if (receipt.backupUtility.configuredImage !== environment.ZITADEL_BACKUP_IMAGE) throw new Error("backup utility differs from reviewed environment");
}
process.stdout.write("identity qualification receipt: ok\n");

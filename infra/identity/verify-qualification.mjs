import { readFileSync } from "node:fs";

if (process.argv.length !== 3) throw new Error("usage: node verify-qualification.mjs RECEIPT.json");
const receipt = JSON.parse(readFileSync(process.argv[2], "utf8"));
const exact = (value, keys, label) => {
  if (!value || typeof value !== "object" || Array.isArray(value) || JSON.stringify(Object.keys(value).sort()) !== JSON.stringify([...keys].sort())) throw new Error(`${label} fields are invalid`);
};
const digest = (value, label) => { if (!/^sha256:[0-9a-f]{64}$/.test(value)) throw new Error(`${label} digest is invalid`); };
exact(receipt, ["schemaVersion", "issuer", "driverDigest", "environmentDigest", "configuredImages", "runningImageDigests", "gates", "backup", "startedAt", "observedAt"], "receipt");
const startedMillis = Date.parse(receipt.startedAt), observedMillis = Date.parse(receipt.observedAt);
if (receipt.schemaVersion !== "blazn.identity.qualification/v2" || !String(receipt.issuer).startsWith("https://") || !Number.isFinite(startedMillis) || !Number.isFinite(observedMillis) || observedMillis < startedMillis) throw new Error("receipt identity or time range is invalid");
digest(receipt.driverDigest, "driver"); digest(receipt.environmentDigest, "environment");
if (!Array.isArray(receipt.configuredImages) || receipt.configuredImages.length !== 4 || new Set(receipt.configuredImages).size !== 4 || receipt.configuredImages.some((value) => !/@sha256:[0-9a-f]{64}$/.test(value))) throw new Error("configured image evidence is invalid");
if (!Array.isArray(receipt.runningImageDigests) || receipt.runningImageDigests.length !== 4 || new Set(receipt.runningImageDigests).size !== 4 || receipt.runningImageDigests.some((value) => !/^sha256:[0-9a-f]{64}$/.test(value))) throw new Error("running image evidence is invalid");
const gateNames = ["composeBootstrap", "tlsIssuer", "oidcDiscovery", "authorizationCodePkce", "verifiedEmail", "reviewedAcrMfa", "legacyLogin", "oidcAwareHealth", "deviceConfirmation"];
exact(receipt.gates, gateNames, "gates");
for (const name of gateNames) { exact(receipt.gates[name], ["evidenceDigest", "observedAt"], `gate ${name}`); digest(receipt.gates[name].evidenceDigest, `gate ${name}`); const gateTime = Date.parse(receipt.gates[name].observedAt); if (!Number.isFinite(gateTime) || gateTime < startedMillis || gateTime > observedMillis + 30_000) throw new Error(`gate ${name} time is invalid or stale`); }
const backupNames = ["manifestDigest", "databaseDigest", "masterKeyBefore", "masterKeyAfter", "patBefore", "patAfter", "preRestorePatSnapshotDigest"];
exact(receipt.backup, backupNames, "backup"); for (const name of backupNames) digest(receipt.backup[name], name);
if (receipt.backup.masterKeyBefore !== receipt.backup.masterKeyAfter || receipt.backup.patBefore !== receipt.backup.patAfter) throw new Error("backup restoration evidence mismatches");
process.stdout.write("identity qualification receipt: ok\n");

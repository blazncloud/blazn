import { readFileSync } from "node:fs";

if (process.argv.length !== 3) throw new Error("usage: node verify-qualification.mjs RECEIPT.json");
const receipt = JSON.parse(readFileSync(process.argv[2], "utf8"));
const exactKeys = (value, keys, label) => {
  if (!value || typeof value !== "object" || Array.isArray(value) || JSON.stringify(Object.keys(value).sort()) !== JSON.stringify([...keys].sort())) throw new Error(`${label} fields are invalid`);
};
exactKeys(receipt, ["schemaVersion", "issuer", "imageDigests", "gates", "backup", "observedAt"], "receipt");
if (receipt.schemaVersion !== "blazn.identity.qualification/v1" || !String(receipt.issuer).startsWith("https://") || !Number.isFinite(Date.parse(receipt.observedAt))) throw new Error("receipt identity is invalid");
if (!Array.isArray(receipt.imageDigests) || receipt.imageDigests.length < 4 || new Set(receipt.imageDigests).size !== receipt.imageDigests.length || receipt.imageDigests.some((value) => !/@sha256:[0-9a-f]{64}$/.test(value))) throw new Error("receipt image digests are invalid");
const gateNames = ["composeBootstrap", "tlsIssuer", "oidcDiscovery", "authorizationCodePkce", "verifiedEmail", "reviewedAcrMfa", "legacyLogin", "oidcAwareHealth", "deviceConfirmation"];
const backupNames = ["databaseRestore", "masterKeyRestore", "patVolumeRestore", "exactImageRollback"];
exactKeys(receipt.gates, gateNames, "gates"); exactKeys(receipt.backup, backupNames, "backup");
for (const name of gateNames) if (receipt.gates[name] !== true) throw new Error(`qualification gate failed: ${name}`);
for (const name of backupNames) if (receipt.backup[name] !== true) throw new Error(`backup gate failed: ${name}`);
process.stdout.write("identity qualification receipt: ok\n");

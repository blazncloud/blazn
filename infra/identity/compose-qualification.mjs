import { readFileSync, writeFileSync } from "node:fs";

if (process.argv.length !== 4) throw new Error("usage: node compose-qualification.mjs DRIVER_EVIDENCE.json RECEIPT.json");
const driver = JSON.parse(readFileSync(process.argv[2], "utf8"));
const startedAt = process.env.QUALIFICATION_STARTED_AT ?? "";
const startedMillis = Date.parse(startedAt);
if (!Number.isFinite(startedMillis) || startedMillis > Date.now() + 30_000) throw new Error("qualification start time is invalid");
const gateNames = ["composeBootstrap", "tlsIssuer", "oidcDiscovery", "authorizationCodePkce", "verifiedEmail", "reviewedAcrMfa", "legacyLogin", "oidcAwareHealth", "deviceConfirmation"];
const exact = (value, keys, label) => {
  if (!value || typeof value !== "object" || Array.isArray(value) || JSON.stringify(Object.keys(value).sort()) !== JSON.stringify([...keys].sort())) throw new Error(`${label} fields are invalid`);
};
exact(driver, ["schemaVersion", "issuer", "gates"], "driver evidence");
if (driver.schemaVersion !== "blazn.identity.driver-evidence/v1" || driver.issuer !== process.env.QUALIFICATION_ISSUER) throw new Error("driver evidence identity mismatch");
exact(driver.gates, gateNames, "driver gates");
for (const name of gateNames) {
  exact(driver.gates[name], ["evidenceDigest", "observedAt"], `driver gate ${name}`);
	const observed = Date.parse(driver.gates[name].observedAt);
	if (!/^sha256:[0-9a-f]{64}$/.test(driver.gates[name].evidenceDigest) || !Number.isFinite(observed) || observed < startedMillis || observed > Date.now() + 30_000) throw new Error(`driver gate ${name} evidence is invalid or stale`);
}
const lines = (name) => (process.env[name] ?? "").split("\n").filter(Boolean).sort();
const digest = (name) => {
  const value = process.env[name] ?? "";
  if (!/^sha256:[0-9a-f]{64}$/.test(value)) throw new Error(`${name} is invalid`);
  return value;
};
const serviceNames = ["postgres", "proxy", "zitadel-api", "zitadel-login"];
const imageRefPattern = /^[a-z0-9][a-z0-9._:/-]*@sha256:[0-9a-f]{64}$/;
const parseObservations = (name) => {
  const result = {};
  for (const line of lines(name)) {
    const [service, configuredImage, containerId, observedConfigImage, imageId, extra] = line.split("|");
    if (extra !== undefined || !serviceNames.includes(service) || result[service]) throw new Error(`${name} service mapping is invalid`);
    if (!imageRefPattern.test(configuredImage) || observedConfigImage !== configuredImage || !/^[0-9a-f]{64}$/.test(containerId) || !/^sha256:[0-9a-f]{64}$/.test(imageId)) throw new Error(`${name} service identity is invalid`);
    result[service] = { containerId, observedConfigImage, imageId };
  }
  exact(result, serviceNames, name);
  return result;
};
const before = parseObservations("QUALIFICATION_SERVICES_BEFORE");
const after = parseObservations("QUALIFICATION_SERVICES_AFTER");
const services = {};
for (const service of serviceNames) {
  const configuredImage = (process.env[`QUALIFICATION_CONFIGURED_${service.replaceAll("-", "_").toUpperCase()}`] ?? "");
  if (!imageRefPattern.test(configuredImage) || before[service].observedConfigImage !== configuredImage || after[service].observedConfigImage !== configuredImage || before[service].imageId !== after[service].imageId || before[service].containerId === after[service].containerId) throw new Error(`rollback service mapping differs for ${service}`);
  services[service] = { configuredImage, before: before[service], after: after[service] };
}
const backupUtility = { configuredImage: process.env.QUALIFICATION_BACKUP_IMAGE ?? "", beforeImageId: process.env.QUALIFICATION_BACKUP_IMAGE_ID_BEFORE ?? "", afterImageId: process.env.QUALIFICATION_BACKUP_IMAGE_ID_AFTER ?? "" };
if (!imageRefPattern.test(backupUtility.configuredImage) || !/^sha256:[0-9a-f]{64}$/.test(backupUtility.beforeImageId) || backupUtility.beforeImageId !== backupUtility.afterImageId) throw new Error("backup utility image identity or rollback comparison is invalid");
const receipt = {
  schemaVersion: "blazn.identity.qualification/v2",
  issuer: process.env.QUALIFICATION_ISSUER,
  driverDigest: digest("QUALIFICATION_DRIVER_DIGEST"),
  environmentDigest: digest("QUALIFICATION_ENVIRONMENT_DIGEST"),
  services,
  backupUtility,
  gates: driver.gates,
  backup: {
    manifestDigest: digest("QUALIFICATION_BACKUP_MANIFEST_DIGEST"),
    databaseDigest: digest("QUALIFICATION_DATABASE_DIGEST"),
    masterKeyBefore: digest("QUALIFICATION_MASTER_BEFORE"),
    masterKeyAfter: digest("QUALIFICATION_MASTER_AFTER"),
    patBefore: digest("QUALIFICATION_PAT_BEFORE"),
    patAfter: digest("QUALIFICATION_PAT_AFTER"),
    preRestorePatSnapshotDigest: digest("QUALIFICATION_PRE_RESTORE_PAT_SNAPSHOT_DIGEST")
  },
	startedAt,
  observedAt: new Date().toISOString()
};
if (receipt.backup.masterKeyBefore !== receipt.backup.masterKeyAfter || receipt.backup.patBefore !== receipt.backup.patAfter) throw new Error("restored master key or PAT digest differs");
writeFileSync(process.argv[3], `${JSON.stringify(receipt, null, 2)}\n`, { encoding: "utf8", mode: 0o600, flag: "wx" });

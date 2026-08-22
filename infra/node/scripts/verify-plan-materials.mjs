import { createHash, createPrivateKey } from "node:crypto";
import { readFile, stat } from "node:fs/promises";

function fail(message) { throw new Error(`node plan material verification failed: ${message}`); }
function exact(value, keys, label) {
  if (!value || typeof value !== "object" || Array.isArray(value)) fail(`${label} must be an object`);
  const actual = Object.keys(value).sort();
  const expected = [...keys].sort();
  if (actual.length !== expected.length || actual.some((key, index) => key !== expected[index])) fail(`${label} is not closed`);
}
const sha256 = (value) => createHash("sha256").update(value).digest("hex");
const canonical = (value) => Array.isArray(value) ? `[${value.map(canonical).join(",")}]`
  : value && typeof value === "object" ? `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${canonical(value[key])}`).join(",")}}`
    : JSON.stringify(value);
const root = process.env.BLAZN_NODE_PLAN_ROOT ?? "/etc/blazn/node-plan";
const sourceTemplates = process.env.BLAZN_NODE_PLAN_SOURCE_TEMPLATES ?? "/opt/blazn-node/templates";
const privateFile = process.env.NODE_PLAN_SIGNING_PRIVATE_KEY_FILE ?? `${root}/signing-private-v1.b64url`;
const metadataFile = `${root}/signing-public-v1.json`;
const templateFile = `${root}/node-install-plan-template-v1.json`;

const [privateText, metadataText, templateText] = await Promise.all([
  readFile(privateFile, "utf8"), readFile(metadataFile, "utf8"), readFile(templateFile, "utf8"),
]);
if (!/^[A-Za-z0-9_-]{43}\n$/.test(privateText)) fail("private seed must be one unpadded base64url line");
const seed = Buffer.from(privateText.trim(), "base64url");
if (seed.length !== 32) fail("private seed must decode to 32 bytes");
const key = createPrivateKey({key: Buffer.concat([Buffer.from("302e020100300506032b657004220420", "hex"), seed]), format: "der", type: "pkcs8"});
const jwk = key.export({format: "jwk"});
if (jwk.d !== privateText.trim() || typeof jwk.x !== "string") fail("private seed is not an Ed25519 key");

const metadata = JSON.parse(metadataText);
exact(metadata, ["schemaVersion", "keyId", "publicKey", "publicKeyFingerprint"], "public metadata");
const fingerprint = `sha256:${sha256(Buffer.from(jwk.x, "base64url"))}`;
if (metadata.schemaVersion !== "blazn.dev/node-plan-signing-key/v1" || metadata.keyId !== "control-plane-node-plan/v1" || metadata.publicKey !== jwk.x || metadata.publicKeyFingerprint !== fingerprint) fail("public metadata does not match private key");

const template = JSON.parse(templateText);
exact(template, ["schemaVersion", "templateId", "profiles"], "template bundle");
if (template.schemaVersion !== "blazn.dev/node-install-plan-templates/v1" || template.templateId !== "frontro-poc-worker/v1") fail("template identity is not frozen");
const profileIds = ["ubuntu-26.04-amd64-worker/v1", "existing-linux-worker-adopt/v1", "macos-lima-worker-adopt/v1"];
exact(template.profiles, profileIds, "template profiles");
const profileKeys = ["cluster", "registryTrust", "components", "nodeService", "labels", "taints", "resourceBounds", "mutations", "validationTests", "rollback"];
const validation = ["binary_digest", "service_active", "node_identity", "cluster_ca", "worker_only", "node_uid_binding", "bootstrap_taint", "capability_heartbeat", "agent_eligibility"];
for (const id of profileIds) {
  const profile = template.profiles[id];
  exact(profile, profileKeys, `profile ${id}`);
  if (profile.cluster.workerOnly !== true || profile.cluster.joinCredentialEndpoint !== "/v1/node-service/join-credentials" || profile.cluster.bootstrapTaint !== "blazn.dev/bootstrap=pending:NoSchedule") fail(`${id} is not worker-only`);
  const wantedValidation = id === "ubuntu-26.04-amd64-worker/v1"
    ? ["binary_digest", "service_account", ...validation.slice(1)]
    : id === "macos-lima-worker-adopt/v1"
      ? [...validation.slice(0, 5), "lima_worker_binding", ...validation.slice(5)]
      : validation;
  if (JSON.stringify(profile.validationTests) !== JSON.stringify(wantedValidation)) fail(`${id} validation gate drifted`);
  if (!Array.isArray(profile.components) || profile.components.length === 0 || !Array.isArray(profile.mutations) || profile.mutations.length === 0) fail(`${id} is incomplete`);
  const names = new Set(profile.components.map((component) => component.name));
  if (names.size !== profile.components.length) fail(`${id} has duplicate component names`);
  for (const component of profile.components) {
    if (!/^[a-f0-9]{64}$/.test(component.sha256)) fail(`${id} has an invalid component digest`);
    if (component.sourceClass === "https") {
      const url = new URL(component.source);
      if (url.protocol !== "https:" || url.hostname !== component.sourceHost) fail(`${id} has an unbound HTTPS component`);
    } else if (!["current_binary", "embedded"].includes(component.sourceClass)
      || ["sourceHost", "source", "repositoryOrigin", "registryHost", "ociReference"].some((key) => key in component)) {
      fail(`${id} has invalid local component provenance`);
    }
  }
  const ordinals = profile.mutations.map((mutation) => mutation.ordinal);
  if (new Set(ordinals).size !== ordinals.length || ordinals.some((ordinal, index) => ordinal !== index + 1)) fail(`${id} mutation ordinals are not contiguous`);
  for (const mutation of profile.mutations) {
    if (mutation.desired?.sourceComponent && !names.has(mutation.desired.sourceComponent)) fail(`${id} mutation references an unknown component`);
    if (mutation.desired?.componentName && !names.has(mutation.desired.componentName)) fail(`${id} mutation references an unknown component`);
    if (mutation.target === "/" || mutation.target.split("/").includes("..")) fail(`${id} mutation target is unsafe`);
    if (["group", "user"].includes(mutation.kind) && mutation.desiredDigest !== `sha256:${sha256(canonical(mutation.desired))}`) fail(`${id} service-account desired digest is not canonical`);
  }
  const mac = id === "macos-lima-worker-adopt/v1";
  if ((mac ? profile.nodeService.manager !== "launchd" : profile.nodeService.manager !== "systemd") || (mac ? profile.nodeService.runAsGroup !== "wheel" : profile.nodeService.runAsGroup !== "blazn-node")) fail(`${id} service boundary drifted`);
  const forbiddenKinds = mac ? ["systemd_unit", "package"] : ["launchd_unit"];
  if (profile.mutations.some((mutation) => forbiddenKinds.includes(mutation.kind))) fail(`${id} contains a cross-platform mutation`);
}

const [systemd, launchd, limaText, binaryDigestsText] = await Promise.all([
  readFile(`${sourceTemplates}/blazn-node.service`),
  readFile(`${sourceTemplates}/com.blazn.node.plist`),
  readFile(`${sourceTemplates}/lima-worker-binding.json`, "utf8"),
  readFile(`${sourceTemplates}/current-binary-digests.json`, "utf8"),
]);
if (sha256(systemd) !== template.profiles["ubuntu-26.04-amd64-worker/v1"].nodeService.definitionSha256 || sha256(systemd) !== template.profiles["existing-linux-worker-adopt/v1"].nodeService.definitionSha256) fail("systemd definition digest drifted");
if (sha256(launchd) !== template.profiles["macos-lima-worker-adopt/v1"].nodeService.definitionSha256) fail("launchd definition digest drifted");
const binaryDigests = JSON.parse(binaryDigestsText);
exact(binaryDigests, ["schemaVersion", "releaseTag", "binaries"], "current binary digest manifest");
exact(binaryDigests.binaries, ["darwin-arm64", "linux-amd64", "linux-arm64"], "current binary platforms");
if (binaryDigests.schemaVersion !== "blazn.dev/current-binary-digests/v1" || binaryDigests.releaseTag !== "v0.1.0-poc.1") fail("current binary release identity drifted");
for (const [profileId, platform] of [["ubuntu-26.04-amd64-worker/v1", "linux-amd64"], ["existing-linux-worker-adopt/v1", "linux-amd64"], ["macos-lima-worker-adopt/v1", "darwin-arm64"]]) {
  const component = template.profiles[profileId].components.find((candidate) => candidate.sourceClass === "current_binary");
  if (!component || component.version !== binaryDigests.releaseTag || component.sha256 !== binaryDigests.binaries[platform]) fail(`${profileId} current binary is not bound to the reviewed executable digest`);
}
const lima = JSON.parse(limaText);
exact(lima, ["schemaVersion", "clusterId", "vmName", "workerName"], "Lima worker binding");
if (lima.schemaVersion !== "blazn.dev/lima-worker-binding/v1" || lima.clusterId !== template.profiles["macos-lima-worker-adopt/v1"].cluster.id || typeof lima.vmName !== "string" || lima.vmName.length === 0 || typeof lima.workerName !== "string" || lima.workerName.length === 0) fail("Lima worker identity drifted");
const canonicalLima = canonical(lima);
const limaSha256 = sha256(canonicalLima);
const macProfile = template.profiles["macos-lima-worker-adopt/v1"];
const limaComponents = macProfile.components.filter((component) => component.name === "lima-worker-binding");
const limaMutations = macProfile.mutations.filter((mutation) => mutation.kind === "file" && mutation.target === "/Library/Application Support/Blazn/lima-worker-binding.json");
if (limaComponents.length !== 1 || limaComponents[0].artifactType !== "configuration" || limaComponents[0].sourceClass !== "embedded" || limaComponents[0].sha256 !== limaSha256) fail("Lima worker component is not bound to its canonical asset");
if (limaMutations.length !== 1 || limaMutations[0].desired?.sourceComponent !== "lima-worker-binding" || limaMutations[0].desired?.contentSha256 !== limaSha256 || limaMutations[0].desiredDigest !== `sha256:${limaSha256}`) fail("Lima worker mutation is not bound to its canonical asset");
for (const path of [privateFile, metadataFile, templateFile]) {
  const info = await stat(path);
  if (!info.isFile()) fail(`${path} is not a regular file`);
}
process.stdout.write(JSON.stringify({status: "ok", keyId: metadata.keyId, publicKeyFingerprint: fingerprint, templateId: template.templateId, templateDigest: `sha256:${sha256(templateText)}`}) + "\n");

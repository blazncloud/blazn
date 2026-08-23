type RecordValue = Record<string, unknown>;
const capabilities = new Set(["task.one-shot","message.follow-up","conversation.resume","event.structured","event.streaming","tool.native","tool.mcp","output.patch","output.artifact","checkpoint.recovery","model.select","provider.configure","approval.prompt","cancel.graceful"]);
const secretKeys = new Set(["token","apitoken","authtoken","sessiontoken","accesstoken","refreshtoken","password","secret","credential","authorization","apikey","privatekey","kubeconfig","nodecredential","bearertoken","clientsecret"]);
const terminal = new Set(["succeeded","failed","cancelled","timed_out","recovery_required"]);
const record = (value: unknown): RecordValue => value !== null && typeof value === "object" && !Array.isArray(value) ? value as RecordValue : {};
const text = (value: unknown): string => typeof value === "string" ? value : "";
const strings = (value: unknown): string[] => Array.isArray(value) && value.every((item) => typeof item === "string") ? value : [];
const normalizedKey = (value: string): string => value.toLowerCase().replace(/[^a-z0-9]/g, "");

export function harnessSecretViolations(value: unknown, path = "$", output: string[] = []): string[] {
  if (Array.isArray(value)) value.forEach((item, index) => harnessSecretViolations(item, `${path}[${index}]`, output));
  else if (value !== null && typeof value === "object") for (const [key, child] of Object.entries(value as RecordValue)) {
    if (secretKeys.has(normalizedKey(key))) output.push(`${path}.${key} is a forbidden raw credential field`);
    harnessSecretViolations(child, `${path}.${key}`, output);
  } else if (typeof value === "string" && (/\bBearer\s+[A-Za-z0-9._~-]{8,}/i.test(value) || /(?:api[_-]?key|token|password|secret|authorization)\s*[:=]\s*\S+/i.test(value) || /-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----/.test(value) || /:\/\/[^/\s]+@/.test(value))) output.push(`${path} contains credential-like material`);
  return output;
}

export function verifyHarnessBundle(value: unknown): string[] {
  const errors = harnessSecretViolations(value), bundle = record(value), definition = record(bundle.definition), version = record(bundle.version), profile = record(bundle.profile);
  if (version.definitionId !== definition.id) errors.push("HarnessVersion does not belong to HarnessDefinition");
  if (profile.harnessVersionId !== version.id) errors.push("HarnessProfile does not select HarnessVersion");
  if (definition.status !== "approved" || profile.status !== "approved") errors.push("Harness definition and profile must be approved");
  const available = strings(version.capabilities);
  if (available.some((item) => !capabilities.has(item))) errors.push("HarnessVersion contains an unknown capability");
  const credentialCapabilities = new Set(strings(version.credentialCapabilities));
  for (const raw of Array.isArray(profile.credentials) ? profile.credentials : []) { const credential=record(raw),capability=text(credential.capability),scope=text(credential.scope),prefix=capability.startsWith("model.")?"route:":capability==="repository.read"?"repo:":"mcp:";if(!credentialCapabilities.has(capability))errors.push("HarnessProfile requests an undeclared credential capability");if(!scope.startsWith(prefix))errors.push("HarnessProfile credential scope does not match its capability"); }
  const proxyProtocols = new Set(strings(record(version.compatibility).proxyProtocols));
  if (!proxyProtocols.has(text(record(profile.model).protocol))) errors.push("HarnessProfile model protocol is unsupported");
  const executable = record(version.executable), argv = strings(executable.fixedArgv);
  if (!text(executable.path).startsWith("/") || argv.some((arg) => /(?:^|[=\s])(?:token|password|secret|authorization|api[_-]?key)(?:[=:]|$)/i.test(decode(arg)))) errors.push("Harness executable contract is unsafe");
  for(const override of Object.values(record(profile.overrides)))if(typeof override==="string"&&/^[A-Za-z0-9_-]{32,}$/.test(override))errors.push("HarnessProfile override resembles raw credential material");
  return errors;
}

function decode(value: string): string { let current=value; for(let i=0;i<3;i++){try{const next=decodeURIComponent(current);if(next===current)break;current=next;}catch{break;}}return current; }

export function verifyAgentCompatibility(agentValue: unknown, bundles: unknown[]): string[] {
  const errors: string[] = harnessSecretViolations(agentValue), root = record(agentValue), agent = record(root.agent), version = record(root.version);
  if (agent.currentVersionId !== version.id || agent.id !== version.agentId || agent.workspaceId !== version.workspaceId) errors.push("Agent and immutable AgentVersion identity mismatch");
  const allowed = new Set(strings(version.allowedHarnessProfileIds));
  if (!allowed.has(text(version.defaultHarnessProfileId))) errors.push("default HarnessProfile is not allowed");
  const suppliedProfiles=bundles.map((item)=>text(record(record(item).profile).id));
  if(suppliedProfiles.length!==new Set(suppliedProfiles).size||JSON.stringify([...new Set(suppliedProfiles)].sort())!==JSON.stringify([...allowed].sort()))errors.push("Agent publication did not validate every allowed HarnessProfile exactly once");
  for (const raw of bundles) {
    errors.push(...verifyHarnessBundle(raw));
    const bundle = record(raw), profile = record(bundle.profile), harnessVersion = record(bundle.version);
    if (!allowed.has(text(profile.id))) { errors.push("HarnessProfile is not allowed by AgentVersion"); continue; }
    if (profile.workspaceId !== version.workspaceId) errors.push("HarnessProfile is outside Agent workspace");
    const definitionPlatforms=new Set(strings(record(bundle.definition).supportedPlatforms));if(strings(harnessVersion.supportedPlatforms).some((platform)=>!definitionPlatforms.has(platform)))errors.push("HarnessVersion platform exceeds HarnessDefinition");
    const available = new Set(strings(harnessVersion.capabilities));
    const missing = strings(version.requiredCapabilities).filter((item) => !available.has(item));
    if (missing.length) errors.push(`HarnessProfile ${text(profile.id)} is missing capabilities: ${missing.join(",")}`);
    if (record(profile.model).routeId !== record(version.modelPolicy).routeId || record(profile.model).routeVersion !== record(version.modelPolicy).routeVersion) errors.push("HarnessProfile model route does not match AgentVersion");
    if (!strings(record(version.modelPolicy).requiredProtocols).includes(text(record(profile.model).protocol))) errors.push("HarnessProfile model protocol is not allowed by AgentVersion");
    const profileTools=new Set(strings(profile.tools));if(strings(version.tools).some((tool)=>!profileTools.has(tool)))errors.push("HarnessProfile omits an AgentVersion tool");
  }
  return errors;
}

export function verifyHarnessRun(agentValue: unknown, bundleValue: unknown, runValue: unknown): string[] {
  const errors = harnessSecretViolations(runValue), agentVersion = record(record(agentValue).version), bundle = record(bundleValue), profile = record(bundle.profile), harnessVersion = record(bundle.version), snapshot = record(runValue), run = record(snapshot.run), session = record(snapshot.session), provenance = record(snapshot.provenance), compatibility = record(snapshot.compatibility);
  if (run.agentVersionId !== agentVersion.id || run.harnessProfileId !== profile.id || run.harnessVersionId !== harnessVersion.id) errors.push("Run does not capture exact AgentVersion and Harness identities");
  if (run.workspaceId !== agentVersion.workspaceId || profile.workspaceId !== run.workspaceId) errors.push("Run resources cross a workspace boundary");
  if (record(provenance.agentVersion).id !== agentVersion.id || record(provenance.agentVersion).digest !== agentVersion.digest || record(provenance.harnessVersion).id !== harnessVersion.id || record(provenance.harnessVersion).digest !== harnessVersion.digest || record(provenance.harnessProfile).id !== profile.id || record(provenance.harnessProfile).digest !== profile.digest) errors.push("Run provenance does not bind immutable Agent and Harness versions");
  if (record(provenance.sandboxTemplateVersion).id !== record(agentVersion.template).versionId || record(provenance.sandboxTemplateVersion).digest !== record(agentVersion.template).digest || record(provenance.modelRoute).id !== record(agentVersion.modelPolicy).routeId || record(provenance.modelRoute).version !== record(agentVersion.modelPolicy).routeVersion) errors.push("Run provenance does not bind exact template and model route");
  if (record(provenance.source).repository !== record(agentVersion.repository).url || record(provenance.source).commit !== record(agentVersion.repository).commit) errors.push("Run source provenance differs from AgentVersion");
  const required = new Set(strings(compatibility.required)), available = new Set(strings(compatibility.available)), missing = strings(compatibility.missing), computedMissing = [...required].filter((item) => !available.has(item)).sort();
  if (JSON.stringify([...missing].sort()) !== JSON.stringify(computedMissing) || compatibility.compatible !== (computedMissing.length === 0)) errors.push("capability compatibility result is inconsistent");
  if(JSON.stringify([...required].sort())!==JSON.stringify(strings(agentVersion.requiredCapabilities).sort())||JSON.stringify([...available].sort())!==JSON.stringify(strings(harnessVersion.capabilities).sort()))errors.push("compatibility did not compare the exact Agent and Harness capability sets");
  if (compatibility.checkedBeforeSandbox !== true || (compatibility.compatible === false && (run.sandboxId !== null || provenance.node !== null || provenance.proxyDecision !== null))) errors.push("capability mismatch did not fail before Sandbox allocation");
  const events = Array.isArray(snapshot.events) ? snapshot.events.map(record) : [], cursors = new Set<string>();
  events.forEach((event,index)=>{if(event.sequence!==index||event.runId!==run.id||event.sessionId!==session.id)errors.push("normalized event identity or sequence is invalid");const cursor=text(event.cursor);if(cursors.has(cursor))errors.push("normalized event cursor is duplicated");cursors.add(cursor);const type=text(event.type),payload=record(event.payload);if(type.startsWith("message.")&&!text(payload.messageId))errors.push("message event lacks message identity");if(type.startsWith("tool.")&&(!text(payload.toolCallId)||!text(payload.tool)))errors.push("tool event lacks normalized tool identity");if(type.startsWith("approval.")&&!text(payload.approvalId))errors.push("approval event lacks approval identity");if((type==="artifact.created"||type==="patch.created")&&!text(payload.artifactId))errors.push("Artifact event lacks Artifact identity");if((type==="cancellation.requested"||type==="cancellation.acknowledged")&&!text(payload.cancellationId))errors.push("cancellation event lacks cancellation identity");});
  if (events.length && session.eventCursor !== events.length-1) errors.push("Session event cursor is stale");
  if (Number(session.followUpCount)!==events.filter((event)=>event.type==="followup.accepted").length) errors.push("follow-up count is not event-bound");
  if (Number(session.resumeGeneration)!==events.filter((event)=>event.type==="harness.resumed").length) errors.push("resume generation is not event-bound");
  const cancellation=record(session.cancellation);if(cancellation.acknowledged===true&&cancellation.requested!==true)errors.push("cancellation was acknowledged without a request");if(cancellation.requested===true&&!events.some((event)=>event.type==="cancellation.requested"))errors.push("cancellation request is not event-bound");if(cancellation.acknowledged===true&&!events.some((event)=>event.type==="cancellation.acknowledged"))errors.push("cancellation acknowledgement is not event-bound");if(run.status==="cancelled"&&(cancellation.acknowledged!==true||cancellation.processTreeTerminated!==true||cancellation.cleanupComplete!==true))errors.push("cancelled Run lacks acknowledged termination and cleanup");
  if (terminal.has(text(run.status))) { const result=record(snapshot.result),last=events.at(-1); if(result.status!==run.status||last?.type!=="result.terminal"||record(last?.payload).status!==run.status)errors.push("terminal Run result or event is incoherent");const artifacts=new Set(strings(result.artifactIds));if(result.patchArtifactId!==null&&!artifacts.has(text(result.patchArtifactId)))errors.push("patch result is absent from Artifact identities"); }
  return errors;
}

export function verifyPortableEvaluation(value: unknown): string[] {
  const errors=harnessSecretViolations(value), root=record(value), required=["discover-capabilities","reject-capability-mismatch-before-sandbox","prepare-no-raw-credentials","one-shot","follow-up","resume","tool-and-approval-events","graceful-cancel-process-tree","terminal-result-artifacts","cleanup-no-secrets"], cases=new Set(strings(root.cases)), result=record(root.result), results=Array.isArray(result.caseResults)?result.caseResults.map(record):[];
  for(const name of required)if(!cases.has(name)||!results.some((item)=>item.case===name&&item.passed===true))errors.push(`portable evaluation did not pass ${name}`);
  const declared=new Set(strings(result.artifactIds));for(const item of results)if(!declared.has(text(item.evidenceArtifactId)))errors.push("conformance result references undeclared evidence");
  if(result.passed!==true||results.some((item)=>item.passed!==true))errors.push("portable evaluation is not fully passing");
  return errors;
}

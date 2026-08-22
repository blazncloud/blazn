#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
contracts="$repo_root/packages/contracts"
fixtures="$contracts/testdata/sandbox"

for document in "$contracts/sandboxes.openapi.json" "$contracts/sandbox-template.schema.json" "$contracts/sandbox-cli-contract.json" "$fixtures"/*.json; do
  jq empty "$document"
done

jq -e '
  .openapi == "3.1.0" and .info.version == "v1alpha1" and
  (.security == [{"bearerAuth":[]}]) and
  (.components.securitySchemes.grantAuth.name == "Authorization") and
  ([.paths[][] | .operationId] as $operations | ($operations | length) == 16 and ($operations | unique | length) == 16) and
  ([.paths[][] | select(.responses.default."$ref" != "#/components/responses/SandboxError")] | length == 0) and
  (.paths["/v1/sandboxes/{sandboxId}/access-grants"].post.responses["201"].headers["Cache-Control"].schema.const == "no-store") and
  (.components.schemas.CreateSandboxRequest.properties.approvedNonSensitive.const == true) and
  (.components.schemas.Sandbox.properties.isolation.const == "approved-non-sensitive-poc") and
  (.components.schemas.SandboxError["x-blazn-error-status"].access_grant_consumed == 410)
' "$contracts/sandboxes.openapi.json" >/dev/null

jq -e '
  .properties.apiVersion.const == "blazn.dev/v1alpha1" and
  .properties.kind.const == "SandboxTemplate" and
  .properties.spec."$ref" == "#/$defs/spec" and
  .["$defs"].spec.additionalProperties == false and
  .["$defs"].spec.properties.policyProfile.const == "poc-restricted-v1" and
  .["$defs"].spec.properties.isolation.const == "approved-non-sensitive-poc" and
  .["$defs"].spec.properties.networkProfile.const == "default-deny-v1" and
  .["$defs"].spec.properties.expiresInSeconds.minimum == 60 and
  .["$defs"].spec.properties.expiresInSeconds.maximum == 7200 and
  .["$defs"].variant.additionalProperties == false and
  (.["$defs"].repository.properties.destination.pattern | startswith("^/workspace/src"))
' "$contracts/sandbox-template.schema.json" >/dev/null

validate_fixture() {
  jq -e '
    .apiVersion == "blazn.dev/v1alpha1" and .kind == "SandboxTemplate" and
    (.metadata.name | test("^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$")) and
    (.spec | keys - ["version","description","policyProfile","isolation","expiresInSeconds","networkProfile","variants","repositories","artifacts"] | length == 0) and
    .spec.policyProfile == "poc-restricted-v1" and .spec.isolation == "approved-non-sensitive-poc" and .spec.networkProfile == "default-deny-v1" and
    (.spec.expiresInSeconds >= 60 and .spec.expiresInSeconds <= 7200) and
    (.spec.variants | length >= 1 and length <= 4) and
    ([.spec.variants[].architecture] | length == (unique | length)) and
    all(.spec.variants[];
      (. | keys - ["name","platform","architecture","imageIndex","imageDigest","command","resources","placementProfile"] | length == 0) and
      .platform == "linux" and (.architecture == "amd64" or .architecture == "arm64") and
      (.imageIndex | test("@sha256:[0-9a-f]{64}$")) and (.imageDigest | test("@sha256:[0-9a-f]{64}$")) and
      (.command | length >= 1 and length <= 32) and
      ((.architecture == "amd64" and .placementProfile == "poc-linux-amd64-v1") or (.architecture == "arm64" and .placementProfile == "poc-mac-arm64-v1"))) and
    all(.spec.repositories[]?; (.url | startswith("https://")) and (.destination | test("^/workspace/src/"))) and
    all(.spec.artifacts[]?; (.path | test("^/workspace/artifacts/")))
  ' "$1" >/dev/null
}

validate_fixture "$fixtures/template-good.json"
if validate_fixture "$fixtures/template-bad-privileged.json"; then
  printf 'unsafe sandbox template fixture unexpectedly validated\n' >&2
  exit 1
fi

jq -e '.contractVersion == "sandbox-cli/v1alpha1" and (.commands | keys | length == 11) and .commands["sandbox exec"].exitCodes.truncated == 9 and .commands["sandbox watch"].stream == "application/x-ndjson" and (.security.forbiddenArgv | index("accessToken") != null)' "$contracts/sandbox-cli-contract.json" >/dev/null
jq -e '.remoteExitCode == 0 and .truncated == false and (has("accessToken") | not)' "$fixtures/cli-exec-success.json" >/dev/null
jq -e '.error.code == "sandbox_not_found" and .exitCode == 1' "$fixtures/cli-error.json" >/dev/null

(cd "$repo_root" && go run ./cmd/generate-sandbox-client --check)
(cd "$repo_root" && go test ./cmd/generate-sandbox-client ./internal/client)

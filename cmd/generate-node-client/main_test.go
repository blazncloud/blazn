package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func checkedInSources(t *testing.T) map[string]source {
	t.Helper()
	root, err := repositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	sources, err := loadSources(root)
	if err != nil {
		t.Fatal(err)
	}
	return sources
}

func cloneDocument(t *testing.T, document map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var clone map[string]any
	if err := json.Unmarshal(encoded, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func TestNodeContractsAndPinnedDigestsMatchGenerator(t *testing.T) {
	sources := checkedInSources(t)
	if err := validateSources(sources, string(nodeTemplate)); err != nil {
		t.Fatal(err)
	}
}

func TestNodeValidatorRejectsMissingAccountReceiptKinds(t *testing.T) {
	sources := checkedInSources(t)
	key := filepath.Join("packages", "contracts", "nodes", "node-install-receipt.schema.json")
	changed := cloneDocument(t, sources[key].doc)
	at(changed, "properties", "mutations", "items", "properties", "kind").(map[string]any)["enum"] = []any{"file"}
	sources[key] = source{path: key, digest: sources[key].digest, doc: changed}
	if err := validateSources(sources, string(nodeTemplate)); err == nil || !strings.Contains(err.Error(), "group and user") {
		t.Fatalf("missing account receipt kinds error=%v", err)
	}
}

func TestNodeValidatorRejectsExchangeIdentityDrift(t *testing.T) {
	sources := checkedInSources(t)
	key := filepath.Join("packages", "contracts", "nodes.openapi.json")
	changed := cloneDocument(t, sources[key].doc)
	at(changed, "components", "schemas", "NodeEnrollmentIdentity").(map[string]any)["required"] = []any{"generation", "expiresAt"}
	sources[key] = source{path: key, digest: sources[key].digest, doc: changed}
	if err := validateSources(sources, string(nodeTemplate)); err == nil || !strings.Contains(err.Error(), "exchange identity") {
		t.Fatalf("exchange identity drift error=%v", err)
	}
}

func TestNodeValidatorRejectsAuthenticationAndOperationDrift(t *testing.T) {
	sources := checkedInSources(t)
	key := filepath.Join("packages", "contracts", "nodes.openapi.json")
	changed := cloneDocument(t, sources[key].doc)
	at(changed, "paths", "/v1/node-enrollments/{enrollmentId}/exchange", "post").(map[string]any)["security"] = []any{map[string]any{"bearerAuth": []any{}}}
	if err := validateOpenAPI(changed); err == nil || !strings.Contains(err.Error(), "disable inherited bearer") {
		t.Fatalf("exchange authentication drift error=%v", err)
	}

	changed = cloneDocument(t, sources[key].doc)
	at(changed, "components", "schemas", "CreateNodeOperationRequest").(map[string]any)["allOf"] = []any{}
	if err := validateOpenAPI(changed); err == nil || !strings.Contains(err.Error(), "discriminators") {
		t.Fatalf("operation discriminator drift error=%v", err)
	}
}

func TestNodeValidatorRejectsSecuritySchemeDrift(t *testing.T) {
	sources := checkedInSources(t)
	key := filepath.Join("packages", "contracts", "nodes.openapi.json")
	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{"bearer scheme", func(document map[string]any) {
			at(document, "components", "securitySchemes", "bearerAuth").(map[string]any)["scheme"] = "basic"
		}, "authentication schemes"},
		{"node proof location", func(document map[string]any) {
			at(document, "components", "securitySchemes", "nodeProof").(map[string]any)["in"] = "query"
		}, "authentication schemes"},
		{"extra scheme", func(document map[string]any) {
			at(document, "components", "securitySchemes").(map[string]any)["unexpected"] = map[string]any{"type": "apiKey"}
		}, "authentication schemes"},
		{"global security", func(document map[string]any) { document["security"] = []any{map[string]any{"nodeProof": []any{}}} }, "global bearer"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			changed := cloneDocument(t, sources[key].doc)
			testCase.mutate(changed)
			if err := validateOpenAPI(changed); err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("security drift error=%v", err)
			}
		})
	}
}

func TestNodeValidatorRejectsNodeErrorDrift(t *testing.T) {
	sources := checkedInSources(t)
	key := filepath.Join("packages", "contracts", "nodes.openapi.json")
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"status", func(document map[string]any) {
			at(document, "components", "schemas", "NodeError", "x-blazn-error-status").(map[string]any)["node_not_found"] = float64(500)
		}},
		{"enum", func(document map[string]any) {
			values := at(document, "components", "schemas", "NodeError", "properties", "code", "enum").([]any)
			at(document, "components", "schemas", "NodeError", "properties", "code").(map[string]any)["enum"] = values[1:]
		}},
		{"required", func(document map[string]any) {
			at(document, "components", "schemas", "NodeError").(map[string]any)["required"] = []any{"code", "message"}
		}},
		{"field", func(document map[string]any) {
			at(document, "components", "schemas", "NodeError", "properties").(map[string]any)["debug"] = map[string]any{"type": "string"}
		}},
		{"bound", func(document map[string]any) {
			at(document, "components", "schemas", "NodeError", "properties", "message").(map[string]any)["maxLength"] = float64(4096)
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			changed := cloneDocument(t, sources[key].doc)
			testCase.mutate(changed)
			if err := validateOpenAPI(changed); err == nil || !strings.Contains(err.Error(), "NodeError") {
				t.Fatalf("NodeError drift error=%v", err)
			}
		})
	}
}

func TestNodeValidatorRejectsUnresolvedRecursiveLocalReference(t *testing.T) {
	sources := checkedInSources(t)
	nodeKey := filepath.Join("packages", "contracts", "nodes.openapi.json")
	commonKey := filepath.Join("packages", "contracts", "openapi.json")
	changed := cloneDocument(t, sources[nodeKey].doc)
	at(changed, "components", "schemas", "WorkerCapacity", "properties", "kubernetesBinding").(map[string]any)["$ref"] = "#/components/schemas/MissingBinding"
	if err := validateSharedNodeErrors(changed, sources[commonKey].doc); err == nil || !strings.Contains(err.Error(), "unresolved local reference") {
		t.Fatalf("unresolved local reference error=%v", err)
	}
}

func TestNodeValidatorRejectsUnresolvedExternalReference(t *testing.T) {
	sources := checkedInSources(t)
	key := filepath.Join("packages", "contracts", "nodes", "node-install-plan.schema.json")
	changed := cloneDocument(t, sources[key].doc)
	changed["$defs"] = map[string]any{"missing": map[string]any{"$ref": "missing.schema.json#/definitions/value"}}
	sources[key] = source{path: key, digest: sources[key].digest, doc: changed}
	if err := validateRecursiveRefs(sources, key, changed, map[string]bool{}); err == nil || !strings.Contains(err.Error(), "unloaded external ref") {
		t.Fatalf("unresolved external reference error=%v", err)
	}
}

func TestNodeValidatorRejectsSharedStatusMismatch(t *testing.T) {
	sources := checkedInSources(t)
	nodeKey := filepath.Join("packages", "contracts", "nodes.openapi.json")
	commonKey := filepath.Join("packages", "contracts", "openapi.json")
	common := cloneDocument(t, sources[commonKey].doc)
	at(common, "components", "schemas", "Error", "x-blazn-error-status").(map[string]any)["unauthorized"] = float64(403)
	if err := validateSharedNodeErrors(sources[nodeKey].doc, common); err == nil || !strings.Contains(err.Error(), "shared error status differs") {
		t.Fatalf("shared status mismatch error=%v", err)
	}
}

func TestNodeValidatorRejectsMissingCommonMiddlewareCode(t *testing.T) {
	sources := checkedInSources(t)
	nodeKey := filepath.Join("packages", "contracts", "nodes.openapi.json")
	commonKey := filepath.Join("packages", "contracts", "openapi.json")
	common := cloneDocument(t, sources[commonKey].doc)
	at(common, "components", "schemas", "Error", "x-blazn-error-status").(map[string]any)["new_common_failure"] = float64(503)
	if err := validateSharedNodeErrors(sources[nodeKey].doc, common); err == nil || !strings.Contains(err.Error(), "shared error status differs") {
		t.Fatalf("missing common code error=%v", err)
	}
}

func TestNodeValidatorRejectsMisnestedCapabilityModels(t *testing.T) {
	sources := checkedInSources(t)
	key := filepath.Join("packages", "contracts", "nodes.openapi.json")
	changed := cloneDocument(t, sources[key].doc)
	properties := at(changed, "components", "schemas", "NodeCapability", "properties").(map[string]any)
	workerProperties := at(changed, "components", "schemas", "WorkerCapacity", "properties").(map[string]any)
	workerProperties["localModels"] = properties["localModels"]
	delete(properties, "localModels")
	if err := validateOpenAPI(changed); err == nil || !strings.Contains(err.Error(), "mis-nested") {
		t.Fatalf("capability nesting drift error=%v", err)
	}
}

func TestNodeTemplateKeepsEnrollmentAndJoinSecretsOutOfURLs(t *testing.T) {
	template := string(nodeTemplate)
	if !strings.Contains(template, "json.Marshal(input)") || !strings.Contains(template, "request.Token") {
		t.Fatal("node enrollment exchange is not JSON-body-backed")
	}
	for _, unsafe := range []string{"PathEscape(request.Token)", `query.Set("token"`, "Authorization\", \"Bearer \"+request.Token"} {
		if strings.Contains(template, unsafe) {
			t.Fatalf("node template contains unsafe token routing: %s", unsafe)
		}
	}
}

func TestNodeTemplateUsesLocalNodeError(t *testing.T) {
	template := string(nodeTemplate)
	for _, marker := range []string{"type NodeError = ErrorBody", "var apiError NodeError", "*NodeError"} {
		if !strings.Contains(template, marker) {
			t.Fatalf("node template lacks local error marker %q", marker)
		}
	}
}

func TestNodeTemplateRequiresRootControlPlaneOriginSemantics(t *testing.T) {
	template := string(nodeTemplate)
	if err := validateTrustedProfileTemplate(template); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		"ControlPlaneOrigin       string",
		"!validNodeControlPlaneOrigin(profile.ControlPlaneOrigin)",
		"port >= 1 && port <= 65535",
	} {
		changed := strings.Replace(template, marker, "", 1)
		if err := validateTrustedProfileTemplate(changed); err == nil || !strings.Contains(err.Error(), "root control-plane origin") {
			t.Fatalf("removed semantic marker %q error=%v", marker, err)
		}
	}
}

func TestNodeContractRequiresDistinctPrivilegedRootState(t *testing.T) {
	sources := checkedInSources(t)
	key := filepath.Join("packages", "contracts", "nodes", "node-install-plan.schema.json")
	plan := sources[key].doc
	if err := validateNodeRootStateContract(plan, string(nodeTemplate)); err != nil {
		t.Fatal(err)
	}
	changed := cloneDocument(t, plan)
	at(changed, "properties", "rollback", "properties", "backupRootClass").(map[string]any)["enum"] = []any{"linux_var_lib", "macos_library_application_support"}
	if err := validateNodeRootStateContract(changed, string(nodeTemplate)); err == nil || !strings.Contains(err.Error(), "privileged root state") {
		t.Fatalf("legacy service-owned roots error=%v", err)
	}
	changed = cloneDocument(t, plan)
	conditions := at(changed, "properties", "rollback", "allOf").([]any)
	conditions[0].(map[string]any)["then"].(map[string]any)["properties"].(map[string]any)["backupRoot"].(map[string]any)["pattern"] = `^/var/lib/blazn/install-backups/`
	if err := validateNodeRootStateContract(changed, string(nodeTemplate)); err == nil || !strings.Contains(err.Error(), "privileged root state") {
		t.Fatalf("service-owned backup path error=%v", err)
	}
}

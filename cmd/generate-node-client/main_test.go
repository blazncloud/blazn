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

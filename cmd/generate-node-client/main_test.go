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

func TestNodeValidatorRejectsMisnestedCapabilityModels(t *testing.T) {
	sources := checkedInSources(t)
	key := filepath.Join("packages", "contracts", "nodes.openapi.json")
	changed := cloneDocument(t, sources[key].doc)
	properties := at(changed, "components", "schemas", "NodeCapability", "properties").(map[string]any)
	healthProperties := at(properties, "health", "properties").(map[string]any)
	healthProperties["localModels"] = properties["localModels"]
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

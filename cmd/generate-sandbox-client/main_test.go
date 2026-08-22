package main

import (
	"path/filepath"
	"testing"
)

func TestPinnedSandboxContractsValidate(t *testing.T) {
	root, err := repositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	sources, err := loadSources(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := validate(sources, string(clientTemplate)); err != nil {
		t.Fatal(err)
	}
}

func TestValidationRejectsMissingOperation(t *testing.T) {
	root, _ := repositoryRoot()
	sources, err := loadSources(root)
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join("packages", "contracts", "sandboxes.openapi.json")
	delete(sources[key].document["paths"].(map[string]any), "/v1/sandboxes/{sandboxId}/access-grants")
	if err := validate(sources, string(clientTemplate)); err == nil {
		t.Fatal("expected a missing-operation error")
	}
}

func TestValidationRejectsOperationFieldDrift(t *testing.T) {
	root, _ := repositoryRoot()
	for name, mutate := range map[string]func(map[string]any){
		"grant idempotency": func(api map[string]any) {
			operation := at(api, "paths", "/v1/sandboxes/{sandboxId}/access-grants", "post").(map[string]any)
			operation["parameters"] = append(operation["parameters"].([]any), map[string]any{"$ref": "#/components/parameters/IdempotencyKey"})
		},
		"raw upload body": func(api map[string]any) {
			schema := at(api, "paths", "/v1/sandbox-access-grants/{grantId}/file", "put", "requestBody", "content", "application/octet-stream", "schema").(map[string]any)
			schema["format"] = "byte"
		},
		"sandbox required field": func(api map[string]any) {
			schema := at(api, "components", "schemas", "Sandbox").(map[string]any)
			required := schema["required"].([]any)
			schema["required"] = required[:len(required)-1]
		},
	} {
		t.Run(name, func(t *testing.T) {
			sources, err := loadSources(root)
			if err != nil {
				t.Fatal(err)
			}
			mutate(sources[filepath.Join("packages", "contracts", "sandboxes.openapi.json")].document)
			if err := validate(sources, string(clientTemplate)); err == nil {
				t.Fatal("expected field-parity rejection")
			}
		})
	}
}

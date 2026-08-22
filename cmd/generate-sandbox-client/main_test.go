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

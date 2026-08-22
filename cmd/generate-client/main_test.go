package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckedInContractMatchesGeneratorAssumptions(t *testing.T) {
	root, err := repositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(filepath.Join(root, "packages", "contracts", "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	if err := validate(document, string(clientTemplate)); err != nil {
		t.Fatal(err)
	}
}

func TestValidatorRejectsBearerAndSchemaDrift(t *testing.T) {
	root, _ := repositoryRoot()
	encoded, _ := os.ReadFile(filepath.Join(root, "packages", "contracts", "openapi.json"))
	var document map[string]any
	_ = json.Unmarshal(encoded, &document)
	security := at(document, "components", "securitySchemes", "bearerAuth").(map[string]any)
	security["bearerFormat"] = "JWT"
	if err := validate(document, string(clientTemplate)); err == nil || !strings.Contains(err.Error(), "opaque") {
		t.Fatalf("bearer drift error = %v", err)
	}
}

func TestRenderIsDeterministicAndEmbedsContractDigest(t *testing.T) {
	first := render(strings.Repeat("a", 64))
	second := render(strings.Repeat("a", 64))
	if string(first) != string(second) || !strings.Contains(string(first), "Contract SHA256: "+strings.Repeat("a", 64)) {
		t.Fatal("render is not deterministic or omitted the digest")
	}
}

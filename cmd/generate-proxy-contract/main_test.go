package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPinnedProxySchemasAndValidator(t *testing.T) {
	root, err := repositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	documents := map[string]map[string]any{}
	for name, pinned := range pinnedDigests {
		encoded, err := os.ReadFile(filepath.Join(root, "packages", "contracts", "proxy", name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(encoded)
		if got := hex.EncodeToString(sum[:]); got != pinned {
			t.Fatalf("%s=%s want=%s", name, got, pinned)
		}
		var document map[string]any
		if err := json.Unmarshal(encoded, &document); err != nil {
			t.Fatal(err)
		}
		documents[name] = document
	}
	if err := validate(documents, string(contractTemplate)); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join(root, "packages", "contracts", "proxy", "fixtures", "poc-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(fixture)
	if got := hex.EncodeToString(sum[:]); got != pinnedPOCPolicyDigest {
		t.Fatalf("fixture=%s want=%s", got, pinnedPOCPolicyDigest)
	}
	if err := validatePOCPolicyFixture(fixture); err != nil {
		t.Fatal(err)
	}
}

func TestPOCPolicyFixtureRejectsRouteSemanticDrift(t *testing.T) {
	root, _ := repositoryRoot()
	fixture, _ := os.ReadFile(filepath.Join(root, "packages", "contracts", "proxy", "fixtures", "poc-policy.json"))
	var policy map[string]any
	if err := json.Unmarshal(fixture, &policy); err != nil {
		t.Fatal(err)
	}
	routes := policy["routes"].([]any)
	routes[0].(map[string]any)["sourceProtocols"] = []any{"openai-chat"}
	mutated, _ := json.Marshal(policy)
	if err := validatePOCPolicyFixture(mutated); err == nil {
		t.Fatal("route source protocol drift accepted")
	}
}

func TestValidatorRejectsContentCaptureAndEventContent(t *testing.T) {
	root, _ := repositoryRoot()
	load := func(name string) map[string]any {
		encoded, _ := os.ReadFile(filepath.Join(root, "packages", "contracts", "proxy", name))
		var value map[string]any
		_ = json.Unmarshal(encoded, &value)
		return value
	}
	documents := map[string]map[string]any{}
	for name := range pinnedDigests {
		documents[name] = load(name)
	}
	documents["policy.schema.json"]["properties"].(map[string]any)["contentCapture"].(map[string]any)["const"] = true
	if err := validate(documents, string(contractTemplate)); err == nil {
		t.Fatal("content capture drift accepted")
	}
	documents["policy.schema.json"] = load("policy.schema.json")
	documents["event.schema.json"]["properties"].(map[string]any)["prompt"] = map[string]any{"type": "string"}
	if err := validate(documents, string(contractTemplate)); err == nil {
		t.Fatal("event content field accepted")
	}
}

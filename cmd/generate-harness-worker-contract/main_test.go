package main

import (
	"bytes"
	"encoding/json"
	"go/format"
	"os"
	"path/filepath"
	"testing"
)

func loadDocument(t *testing.T, path string) map[string]any {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func TestSchemasAndGeneratedOutputArePinned(t *testing.T) {
	root, err := repositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	harness := loadDocument(t, filepath.Join(root, "packages/contracts/harness-worker.schema.json"))
	scope := loadDocument(t, filepath.Join(root, "packages/contracts/proxy/workload-scope.schema.json"))
	if err := validateSchemas(harness, scope); err != nil {
		t.Fatal(err)
	}
	generated, err := format.Source(render())
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(filepath.Join(root, "internal/harnessworker/contracts.gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, generated) {
		t.Fatal("generated harness worker contracts are stale")
	}
}

func TestSchemaValidationRejectsRouteAndSecretBoundaryDrift(t *testing.T) {
	root, err := repositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	harness := loadDocument(t, filepath.Join(root, "packages/contracts/harness-worker.schema.json"))
	scope := loadDocument(t, filepath.Join(root, "packages/contracts/proxy/workload-scope.schema.json"))
	properties := valueAt(scope, "properties").(map[string]any)
	delete(properties, "protocol")
	if err := validateSchemas(harness, scope); err == nil {
		t.Fatal("route protocol drift passed validation")
	}
	scope = loadDocument(t, filepath.Join(root, "packages/contracts/proxy/workload-scope.schema.json"))
	properties = valueAt(scope, "properties").(map[string]any)
	delete(properties, "harnessExecutableDigest")
	if err := validateSchemas(harness, scope); err == nil {
		t.Fatal("harness executable binding drift passed validation")
	}
	scope = loadDocument(t, filepath.Join(root, "packages/contracts/proxy/workload-scope.schema.json"))
	boundary := valueAt(scope, "x-blazn-secret-boundary").(map[string]any)
	boundary["listenerTokenDelivery"] = "inline"
	if err := validateSchemas(harness, scope); err == nil {
		t.Fatal("inline listener token delivery passed validation")
	}
}

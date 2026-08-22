package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPinnedRunContractMatchesTemplate(t *testing.T) {
	root, err := repositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(filepath.Join(root, "packages", "contracts", "runs.openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	if err := validate(document, string(runTemplate)); err != nil {
		t.Fatal(err)
	}
}

func TestRunGeneratorRejectsOperationAndSchemaDrift(t *testing.T) {
	root, _ := repositoryRoot()
	load := func() map[string]any {
		encoded, err := os.ReadFile(filepath.Join(root, "packages", "contracts", "runs.openapi.json"))
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := json.Unmarshal(encoded, &document); err != nil {
			t.Fatal(err)
		}
		return document
	}
	t.Run("operation", func(t *testing.T) {
		document := load()
		delete(valueAt(document, "paths", "/v1/workspaces/{workspaceId}/projects/{projectId}/runs").(map[string]any), "post")
		if err := validate(document, string(runTemplate)); err == nil {
			t.Fatal("missing create operation passed")
		}
	})
	t.Run("field", func(t *testing.T) {
		document := load()
		delete(valueAt(document, "components", "schemas", "Run", "properties").(map[string]any), "workspaceId")
		if err := validate(document, string(runTemplate)); err == nil {
			t.Fatal("missing tenant field passed")
		}
	})
	t.Run("required", func(t *testing.T) {
		document := load()
		schema := valueAt(document, "components", "schemas", "Run").(map[string]any)
		schema["required"] = []any{"id"}
		if err := validate(document, string(runTemplate)); err == nil {
			t.Fatal("required tenant fields drift passed")
		}
	})
	t.Run("enum", func(t *testing.T) {
		document := load()
		valueAt(document, "components", "schemas", "ProofClass").(map[string]any)["enum"] = []any{"synthetic"}
		if err := validate(document, string(runTemplate)); err == nil {
			t.Fatal("proof class enum drift passed")
		}
	})
}

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPinnedAgentHarnessOperations(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "packages", "contracts", "agent-harness.openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if err := validate(document); err != nil {
		t.Fatal(err)
	}
	paths := document["paths"].(map[string]any)
	delete(paths, "/v1/workspaces/{workspaceId}/agents")
	if err := validate(document); err == nil {
		t.Fatal("missing operation was accepted")
	}
}

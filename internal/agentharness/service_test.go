package agentharness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blazncloud/blazn/internal/client"
)

func fixtureDocument(t *testing.T, path, key string) client.JSONDocument {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err = json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	if key == "" {
		return client.JSONDocument(root)
	}
	value, ok := root[key].(map[string]any)
	if !ok {
		t.Fatalf("fixture key %s missing", key)
	}
	return client.JSONDocument(value)
}
func cloneDocument(t *testing.T, d client.JSONDocument) client.JSONDocument {
	t.Helper()
	data, _ := json.Marshal(d)
	var out client.JSONDocument
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestValidateDocumentUsesNormativeSchemas(t *testing.T) {
	agent := fixtureDocument(t, "../../packages/contracts/testdata/harness/agent-good.json", "version")
	profile := fixtureDocument(t, "../../packages/contracts/testdata/harness/codex-profile.json", "profile")
	if err := ValidateDocument("agent-version", agent); err != nil {
		t.Fatalf("valid AgentVersion: %v", err)
	}
	if err := ValidateDocument("harness-profile", profile); err != nil {
		t.Fatalf("valid HarnessProfile: %v", err)
	}
	tests := []struct {
		name, kind string
		document   client.JSONDocument
	}{
		{"malformed UUID", "agent-version", cloneDocument(t, agent)},
		{"malformed digest", "agent-version", cloneDocument(t, agent)},
		{"invalid nested object", "harness-profile", cloneDocument(t, profile)},
		{"extra field", "harness-profile", cloneDocument(t, profile)},
	}
	tests[0].document["id"] = "NOT-A-UUID"
	tests[1].document["digest"] = "sha256:xyz"
	tests[2].document["policy"].(map[string]any)["network"] = "unbounded"
	tests[3].document["unexpected"] = true
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateDocument(tc.kind, tc.document); err == nil || !strings.Contains(err.Error(), "normative schema") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestReadDocumentRejectsTrailingJSONAndPreservesPathErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(p, []byte(`{} garbage`), 0600)
	if _, e := ReadDocument(p); e == nil || !strings.Contains(e.Error(), "trailing JSON") {
		t.Fatalf("trailing err=%v", e)
	}
	missing := filepath.Join(t.TempDir(), "missing.json")
	if _, e := ReadDocument(missing); e == nil || !strings.Contains(e.Error(), "missing.json") {
		t.Fatalf("missing err=%v", e)
	}
}

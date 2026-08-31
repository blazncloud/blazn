package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var expected = map[string]string{
	"GET /v1/workspaces/{workspaceId}/agents": "listAgents", "POST /v1/workspaces/{workspaceId}/agents": "createAgent",
	"GET /v1/workspaces/{workspaceId}/agents/{agentId}": "getAgent", "GET /v1/workspaces/{workspaceId}/agents/{agentId}/versions": "listAgentVersions", "POST /v1/workspaces/{workspaceId}/agents/{agentId}/versions": "publishAgentVersion", "GET /v1/workspaces/{workspaceId}/agents/{agentId}/versions/{versionId}": "getAgentVersion",
	"GET /v1/workspaces/{workspaceId}/harness/definitions": "listHarnessDefinitions", "POST /v1/workspaces/{workspaceId}/harness/definitions": "createHarnessDefinition", "GET /v1/workspaces/{workspaceId}/harness/definitions/{definitionId}": "getHarnessDefinition", "GET /v1/workspaces/{workspaceId}/harness/definitions/{definitionId}/versions": "listHarnessVersions", "POST /v1/workspaces/{workspaceId}/harness/definitions/{definitionId}/versions": "publishHarnessVersion", "GET /v1/workspaces/{workspaceId}/harness/definitions/{definitionId}/versions/{versionId}": "getHarnessVersion",
	"GET /v1/workspaces/{workspaceId}/harness/profiles": "listHarnessProfiles", "POST /v1/workspaces/{workspaceId}/harness/profiles": "createHarnessProfile", "GET /v1/workspaces/{workspaceId}/harness/profiles/{profileId}": "getHarnessProfile", "POST /v1/workspaces/{workspaceId}/harness/profiles/{profileId}/revisions": "reviseHarnessProfile",
}

func main() {
	check := flag.Bool("check", false, "check generated Agent/Harness client")
	flag.Parse()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	contract, err := os.ReadFile(filepath.Join(root, "packages/contracts/agent-harness.openapi.json"))
	fatal(err)
	var doc map[string]any
	fatal(json.Unmarshal(contract, &doc))
	fatal(validate(doc))
	digest := sha256.Sum256(contract)
	marker := "// Contract SHA256: " + hex.EncodeToString(digest[:])
	clientPath := filepath.Join(root, "internal/client/agent_harness.gen.go")
	client, err := os.ReadFile(clientPath)
	fatal(err)
	lines := strings.Split(string(client), "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[1], "// Contract SHA256: ") {
		fatal(fmt.Errorf("generated Agent/Harness client has no contract marker"))
	}
	if *check {
		if lines[1] != marker {
			fatal(fmt.Errorf("generated Agent/Harness client is stale; run make generate-agent-harness-client"))
		}
		return
	}
	lines[1] = marker
	fatal(os.WriteFile(clientPath, []byte(strings.Join(lines, "\n")), 0o644))
}

func validate(doc map[string]any) error {
	if doc["openapi"] != "3.1.0" {
		return fmt.Errorf("Agent/Harness OpenAPI version changed")
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		return fmt.Errorf("Agent/Harness paths missing")
	}
	seen := map[string]string{}
	for path, raw := range paths {
		methods, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid path %s", path)
		}
		for method, op := range methods {
			object, ok := op.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid operation")
			}
			seen[strings.ToUpper(method)+" "+path], _ = object["operationId"].(string)
		}
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("Agent/Harness operations changed")
	}
	for key, want := range expected {
		if seen[key] != want {
			return fmt.Errorf("operation %s=%q want %q", key, seen[key], want)
		}
	}
	return nil
}
func fatal(err error) {
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

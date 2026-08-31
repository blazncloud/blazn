package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

//go:embed agent_harness.gen.go.tmpl
var clientTemplate []byte

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
	agentSchema, err := os.ReadFile(filepath.Join(root, "packages/contracts/agent.schema.json"))
	fatal(err)
	harnessSchema, err := os.ReadFile(filepath.Join(root, "packages/contracts/harness.schema.json"))
	fatal(err)
	digestInput := append(append(append([]byte{}, contract...), agentSchema...), harnessSchema...)
	digest := sha256.Sum256(digestInput)
	generated := bytes.ReplaceAll(clientTemplate, []byte("{{CONTRACT_SHA256}}"), []byte(hex.EncodeToString(digest[:])))
	clientPath := filepath.Join(root, "internal/client/agent_harness.gen.go")
	schemaGenerated := []byte("// Code generated from normative Agent/Harness schemas; DO NOT EDIT.\npackage agentharness\n\nconst agentSchemaJSON = " + strconv.Quote(string(agentSchema)) + "\nconst harnessSchemaJSON = " + strconv.Quote(string(harnessSchema)) + "\n")
	schemaPath := filepath.Join(root, "internal/agentharness/schemas.gen.go")
	if *check {
		current, err := os.ReadFile(clientPath)
		fatal(err)
		if !bytes.Equal(current, generated) {
			fatal(fmt.Errorf("generated Agent/Harness client is stale; run make generate-agent-harness-client"))
		}
		currentSchema, err := os.ReadFile(schemaPath)
		fatal(err)
		if !bytes.Equal(currentSchema, schemaGenerated) {
			fatal(fmt.Errorf("generated Agent/Harness schemas are stale; run make generate-agent-harness-client"))
		}
		return
	}
	fatal(os.WriteFile(clientPath, generated, 0o644))
	fatal(os.WriteFile(schemaPath, schemaGenerated, 0o644))
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

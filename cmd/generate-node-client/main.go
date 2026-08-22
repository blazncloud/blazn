package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

//go:embed node.gen.go.tmpl
var nodeTemplate []byte

const (
	openAPISHA256 = "bfe79594daa71b4be9a6ec7555fe92de495304943f4ca130bcffc360e4c89158"
	planSHA256    = "663d02a35bd912f713e7139a7ebe452525a7baa8a3a24a1da250a28b75496eb7"
	receiptSHA256 = "5618b6ddfec05f8c9e0b84b76774a24abc4be3b145de7707aa0ab675f1ee082c"
)

type source struct {
	path   string
	digest string
	doc    map[string]any
}

type operation struct {
	path, method, id, success, requestRef, responseRef string
	security                                           string
}

var operations = []operation{
	{"/v1/workspaces/{workspaceId}/node-enrollments", "post", "createNodeEnrollment", "201", "CreateNodeEnrollmentRequest", "NodeEnrollmentSecret", "bearer"},
	{"/v1/node-enrollments/{enrollmentId}/exchange", "post", "exchangeNodeEnrollment", "200", "ExchangeNodeEnrollmentRequest", "nodes/node-install-plan.schema.json", "public"},
	{"/v1/workspaces/{workspaceId}/nodes", "get", "listNodes", "200", "", "inline", "bearer"},
	{"/v1/nodes/{nodeId}", "get", "getNode", "200", "", "Node", "bearer"},
	{"/v1/nodes/{nodeId}/operations", "post", "createNodeOperation", "202", "CreateNodeOperationRequest", "NodeOperation", "bearer"},
	{"/v1/nodes/{nodeId}/events", "get", "streamNodeEvents", "200", "", "sse", "bearer"},
	{"/v1/node-service/heartbeats", "post", "submitNodeHeartbeat", "204", "NodeHeartbeat", "", "nodeProof"},
	{"/v1/node-service/join-credentials", "post", "issueNodeJoinCredential", "200", "JoinCredentialRequest", "JoinCredential", "nodeProof"},
}

func main() {
	check := flag.Bool("check", false, "fail if the checked-in node client differs")
	flag.Parse()
	root, err := repositoryRoot()
	fatalIf(err)
	sources, err := loadSources(root)
	fatalIf(err)
	fatalIf(validateSources(sources, string(nodeTemplate)))
	generated := bytes.ReplaceAll(nodeTemplate, []byte("OPENAPI_SHA256"), []byte(openAPISHA256))
	generated = bytes.ReplaceAll(generated, []byte("PLAN_SHA256"), []byte(planSHA256))
	generated = bytes.ReplaceAll(generated, []byte("RECEIPT_SHA256"), []byte(receiptSHA256))
	generated, err = format.Source(generated)
	fatalIf(err)
	output := filepath.Join(root, "internal", "client", "node.gen.go")
	if *check {
		current, err := os.ReadFile(output)
		fatalIf(err)
		if !bytes.Equal(current, generated) {
			fatalIf(fmt.Errorf("generated node client is stale; run 'make generate-node-client'"))
		}
		return
	}
	temp, err := os.CreateTemp(filepath.Dir(output), ".node.gen.go.*")
	fatalIf(err)
	name := temp.Name()
	defer os.Remove(name)
	_, err = temp.Write(generated)
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	fatalIf(err)
	fatalIf(os.Rename(name, output))
}

func loadSources(root string) (map[string]source, error) {
	definitions := []source{
		{path: filepath.Join("packages", "contracts", "nodes.openapi.json"), digest: openAPISHA256},
		{path: filepath.Join("packages", "contracts", "nodes", "node-install-plan.schema.json"), digest: planSHA256},
		{path: filepath.Join("packages", "contracts", "nodes", "node-install-receipt.schema.json"), digest: receiptSHA256},
	}
	result := make(map[string]source, len(definitions))
	for _, definition := range definitions {
		encoded, err := os.ReadFile(filepath.Join(root, definition.path))
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(encoded)
		got := hex.EncodeToString(digest[:])
		if got != definition.digest {
			return nil, fmt.Errorf("%s digest %s is not represented by pinned node models %s", definition.path, got, definition.digest)
		}
		if err := json.Unmarshal(encoded, &definition.doc); err != nil {
			return nil, fmt.Errorf("decode %s: %w", definition.path, err)
		}
		result[definition.path] = definition
	}
	return result, nil
}

func validateSources(sources map[string]source, template string) error {
	openAPI := sources[filepath.Join("packages", "contracts", "nodes.openapi.json")].doc
	if err := validateOpenAPI(openAPI); err != nil {
		return err
	}
	plan := sources[filepath.Join("packages", "contracts", "nodes", "node-install-plan.schema.json")].doc
	receipt := sources[filepath.Join("packages", "contracts", "nodes", "node-install-receipt.schema.json")].doc
	if err := validateInstallSchema(plan, "Blazn NodeInstallPlan", []string{"schemaVersion", "planId", "nodeId", "enrollmentId", "workspaceId", "mode", "cluster", "target", "components", "mutations", "rollback", "issuedAt", "expiresAt", "signingKeyId", "digest", "signature"}); err != nil {
		return fmt.Errorf("install plan: %w", err)
	}
	if err := validateInstallSchema(receipt, "Blazn NodeInstallReceipt", []string{"schemaVersion", "receiptId", "planId", "planDigest", "nodeId", "generation", "state", "owner", "binary", "service", "mutations", "createdAt", "updatedAt", "checksum"}); err != nil {
		return fmt.Errorf("install receipt: %w", err)
	}
	for _, marker := range []string{
		"func ValidateNodeInstallPlan(", "func ValidateNodeInstallReceipt(",
		`Header.Set("Authorization", "Bearer "+accessToken)`,
		`Header.Set("X-Blazn-Node-Proof", nodeProof)`,
		`Header.Set("Idempotency-Key", idempotencyKey)`,
		`Header.Set("Last-Event-ID", lastEventID)`,
	} {
		if !strings.Contains(template, marker) {
			return fmt.Errorf("node template lacks required marker %q", marker)
		}
	}
	for _, unsafe := range []string{"PathEscape(request.Token)", `query.Set("token"`, "Authorization\", \"Bearer \"+request.Token"} {
		if strings.Contains(template, unsafe) {
			return fmt.Errorf("enrollment token enters an unsafe transport surface: %q", unsafe)
		}
	}
	return nil
}

func validateOpenAPI(document map[string]any) error {
	if atString(document, "openapi") != "3.1.0" || atString(document, "info", "version") != "nodes/v1alpha1" {
		return fmt.Errorf("node overlay must be OpenAPI 3.1.0 nodes/v1alpha1")
	}
	servers, _ := at(document, "servers").([]any)
	if len(servers) != 1 || atString(servers[0], "url") != "https://blazn.benpelo.com" {
		return fmt.Errorf("node API server origin changed")
	}
	if atString(document, "components", "securitySchemes", "bearerAuth", "bearerFormat") != "opaque" || atString(document, "components", "securitySchemes", "nodeProof", "name") != "X-Blazn-Node-Proof" {
		return fmt.Errorf("node authentication schemes changed")
	}
	paths, _ := at(document, "paths").(map[string]any)
	if len(paths) != len(operations) {
		return fmt.Errorf("node paths changed: got %d want %d", len(paths), len(operations))
	}
	for _, expected := range operations {
		base := []string{"paths", expected.path, expected.method}
		if atString(document, append(base, "operationId")...) != expected.id {
			return fmt.Errorf("%s %s operationId changed", expected.method, expected.path)
		}
		if expected.requestRef != "" && atString(document, append(base, "requestBody", "content", "application/json", "schema", "$ref")...) != "#/components/schemas/"+expected.requestRef {
			return fmt.Errorf("%s request schema changed", expected.id)
		}
		if atString(document, append(base, "responses", "default", "$ref")...) != "#/components/responses/Error" {
			return fmt.Errorf("%s default error changed", expected.id)
		}
		switch expected.responseRef {
		case "":
		case "inline":
			if atString(document, append(base, "responses", expected.success, "content", "application/json", "schema", "type")...) != "object" {
				return fmt.Errorf("%s list response changed", expected.id)
			}
		case "sse":
			if atString(document, append(base, "responses", expected.success, "content", "text/event-stream", "schema", "type")...) != "string" {
				return fmt.Errorf("%s SSE response changed", expected.id)
			}
		case "nodes/node-install-plan.schema.json":
			if atString(document, append(base, "responses", expected.success, "content", "application/json", "schema", "$ref")...) != expected.responseRef {
				return fmt.Errorf("%s install plan response changed", expected.id)
			}
		default:
			if atString(document, append(base, "responses", expected.success, "content", "application/json", "schema", "$ref")...) != "#/components/schemas/"+expected.responseRef {
				return fmt.Errorf("%s response schema changed", expected.id)
			}
		}
		security := at(document, append(base, "security")...)
		switch expected.security {
		case "public":
			values, ok := security.([]any)
			if !ok || len(values) != 0 {
				return fmt.Errorf("%s must explicitly disable inherited bearer auth", expected.id)
			}
		case "nodeProof":
			if fmt.Sprint(security) != "[map[nodeProof:[]]]" {
				return fmt.Errorf("%s must require only nodeProof", expected.id)
			}
		case "bearer":
			if security != nil {
				return fmt.Errorf("%s must inherit global bearer auth", expected.id)
			}
		}
	}
	if atString(document, "components", "schemas", "ExchangeNodeEnrollmentRequest", "properties", "token", "type") != "string" {
		return fmt.Errorf("enrollment token must remain in JSON request body")
	}
	capability, ok := at(document, "components", "schemas", "NodeCapability").(map[string]any)
	if !ok || capability["additionalProperties"] != false {
		return fmt.Errorf("NodeCapability must remain a closed object")
	}
	for _, field := range []string{"version", "platform", "architecture", "cpu", "memoryBytes", "diskBytes", "accelerators", "labels", "limits", "health", "sandboxBackends", "runtimeClasses", "localModels"} {
		if at(capability, "properties", field) == nil {
			return fmt.Errorf("NodeCapability.%s is missing or mis-nested", field)
		}
	}
	if atString(document, "components", "schemas", "NodeCapability", "properties", "localModels", "items", "$ref") != "#/components/schemas/LocalModelCapability" || at(document, "components", "schemas", "LocalModelCapability") == nil {
		return fmt.Errorf("LocalModelCapability must be a top-level component referenced by NodeCapability")
	}
	discriminators, ok := at(document, "components", "schemas", "CreateNodeOperationRequest", "allOf").([]any)
	if !ok || len(discriminators) != 6 {
		return fmt.Errorf("node operation discriminators changed")
	}
	return nil
}

func validateInstallSchema(document map[string]any, title string, requiredFields []string) error {
	if atString(document, "$schema") != "https://json-schema.org/draft/2020-12/schema" || atString(document, "title") != title || atString(document, "type") != "object" || at(document, "additionalProperties") != false {
		return fmt.Errorf("must remain a closed JSON Schema 2020-12 object named %q", title)
	}
	required, _ := at(document, "required").([]any)
	got := make([]string, 0, len(required))
	for _, value := range required {
		name, ok := value.(string)
		if !ok {
			return fmt.Errorf("required contains a non-string")
		}
		got = append(got, name)
	}
	sort.Strings(got)
	want := append([]string(nil), requiredFields...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		return fmt.Errorf("required fields changed: got %v want %v", got, want)
	}
	if atString(document, "properties", "schemaVersion", "const") != "nodes/v1alpha1" {
		return fmt.Errorf("schemaVersion changed")
	}
	if atNumber(document, "properties", "mutations", "maxItems") != 256 || at(document, "properties", "mutations", "items", "additionalProperties") != false {
		return fmt.Errorf("mutation bounds or closure changed")
	}
	return nil
}

func at(value any, path ...string) any {
	current := value
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[key]
	}
	return current
}

func atString(value any, path ...string) string {
	result, _ := at(value, path...).(string)
	return result
}

func atNumber(value any, path ...string) float64 {
	result, _ := at(value, path...).(float64)
	return result
}

func repositoryRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("resolve generator source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..")), nil
}

func fatalIf(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

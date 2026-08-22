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
	"strconv"
	"strings"
)

//go:embed sandbox.gen.go.tmpl
var clientTemplate []byte

const (
	openAPISHA256  = "ccb86d200b07b91de21b0447da0e5cc761300cb35b38046a2f01593bfb8965d0"
	templateSHA256 = "e555682663c8c45c6813d65faf1937d5a860e670f2c816fdf31b7fbb96f932e1"
	cliSHA256      = "83eeeb9d7574e4956acdd0026279f6be7b6fbcad16f0b3450a8d541459333b35"
)

type source struct {
	path, digest string
	document     map[string]any
}

type operationSpec struct {
	path, method, id, success, requestRef, requestMedia, responseRef, responseMedia, security string
	parameters                                                                                []string
}

var sandboxOperations = []operationSpec{
	{"/v1/workspaces/{workspaceId}/sandbox-templates", "post", "createSandboxTemplate", "201", "sandbox-template.schema.json", "application/json", "SandboxTemplateEnvelope", "application/json", "bearer", []string{"WorkspaceId", "IdempotencyKey"}},
	{"/v1/workspaces/{workspaceId}/sandbox-templates", "get", "listSandboxTemplates", "200", "", "", "SandboxTemplateList", "application/json", "bearer", []string{"WorkspaceId", "Cursor"}},
	{"/v1/sandbox-templates/{templateId}", "get", "getSandboxTemplate", "200", "", "", "SandboxTemplateEnvelope", "application/json", "bearer", []string{"TemplateId"}},
	{"/v1/sandbox-templates/{templateId}/draft", "put", "replaceSandboxTemplateDraft", "200", "ReplaceTemplateDraftRequest", "application/json", "SandboxTemplateEnvelope", "application/json", "bearer", []string{"TemplateId", "IdempotencyKey"}},
	{"/v1/sandbox-templates/{templateId}/versions", "post", "publishSandboxTemplateVersion", "201", "PublishTemplateVersionRequest", "application/json", "SandboxTemplateVersionEnvelope", "application/json", "bearer", []string{"TemplateId", "IdempotencyKey"}},
	{"/v1/sandbox-templates/{templateId}/versions", "get", "listSandboxTemplateVersions", "200", "", "", "SandboxTemplateVersionList", "application/json", "bearer", []string{"TemplateId", "Cursor"}},
	{"/v1/sandbox-template-versions/{versionId}", "get", "getSandboxTemplateVersion", "200", "", "", "SandboxTemplateVersionEnvelope", "application/json", "bearer", []string{"VersionId"}},
	{"/v1/workspaces/{workspaceId}/sandboxes", "post", "createSandbox", "202", "CreateSandboxRequest", "application/json", "SandboxMutation", "application/json", "bearer", []string{"WorkspaceId", "IdempotencyKey"}},
	{"/v1/workspaces/{workspaceId}/sandboxes", "get", "listSandboxes", "200", "", "", "SandboxList", "application/json", "bearer", []string{"WorkspaceId", "Cursor"}},
	{"/v1/sandboxes/{sandboxId}", "get", "getSandbox", "200", "", "", "Sandbox", "application/json", "bearer", []string{"SandboxId"}},
	{"/v1/sandboxes/{sandboxId}/events", "get", "streamSandboxEvents", "200", "", "", "SandboxEvent", "text/event-stream", "bearer", []string{"SandboxId", "LastEventId"}},
	{"/v1/sandboxes/{sandboxId}/operations", "post", "createSandboxOperation", "202", "CreateSandboxOperationRequest", "application/json", "SandboxMutation", "application/json", "bearer", []string{"SandboxId", "IdempotencyKey"}},
	{"/v1/sandboxes/{sandboxId}/access-grants", "post", "createSandboxAccessGrant", "201", "CreateAccessGrantRequest", "application/json", "AccessGrantCreated", "application/json", "bearer", []string{"SandboxId"}},
	{"/v1/sandbox-access-grants/{grantId}/exec", "post", "executeSandboxGrant", "200", "ExecRequest", "application/json", "ExecResult", "application/json", "grant", []string{"GrantId"}},
	{"/v1/sandbox-access-grants/{grantId}/file", "put", "uploadSandboxGrantFile", "200", "binary", "application/octet-stream", "FileTransferResult", "application/json", "grant", []string{"GrantId", "SandboxPath", "ContentSize", "ContentSHA256"}},
	{"/v1/sandbox-access-grants/{grantId}/file", "get", "downloadSandboxGrantFile", "200", "", "", "binary", "application/octet-stream", "grant", []string{"GrantId", "SandboxPath"}},
	{"/v1/sandbox-operations/{operationId}", "get", "getSandboxOperation", "200", "", "", "SandboxOperation", "application/json", "bearer", []string{"OperationId"}},
	{"/v1/sandboxes/{sandboxId}/artifacts", "get", "listSandboxArtifacts", "200", "", "", "SandboxArtifactList", "application/json", "bearer", []string{"SandboxId", "Cursor"}},
	{"/v1/sandbox-artifacts/{artifactId}", "get", "getSandboxArtifact", "200", "", "", "SandboxArtifact", "application/json", "bearer", []string{"ArtifactId"}},
	{"/v1/sandbox-artifacts/{artifactId}/content", "get", "downloadSandboxArtifact", "200", "", "", "binary", "application/octet-stream", "bearer", []string{"ArtifactId"}},
}

func main() {
	check := flag.Bool("check", false, "fail if the checked-in sandbox client differs")
	flag.Parse()
	root, err := repositoryRoot()
	fatalIf(err)
	sources, err := loadSources(root)
	fatalIf(err)
	fatalIf(validate(sources, string(clientTemplate)))
	openapi := sources[filepath.Join("packages", "contracts", "sandboxes.openapi.json")].document
	generated := bytes.ReplaceAll(clientTemplate, []byte("SANDBOX_OPENAPI_SHA256"), []byte(openAPISHA256))
	generated = bytes.ReplaceAll(generated, []byte("SANDBOX_TEMPLATE_SHA256"), []byte(templateSHA256))
	generated = bytes.ReplaceAll(generated, []byte("SANDBOX_CLI_SHA256"), []byte(cliSHA256))
	generated = bytes.ReplaceAll(generated, []byte("SANDBOX_ERROR_STATUSES"), []byte(errorStatusSource(openapi)))
	generated, err = format.Source(generated)
	fatalIf(err)
	output := filepath.Join(root, "internal", "client", "sandbox.gen.go")
	if *check {
		current, err := os.ReadFile(output)
		fatalIf(err)
		if !bytes.Equal(current, generated) {
			fatalIf(fmt.Errorf("generated sandbox client is stale; run 'make generate-sandbox-client'"))
		}
		return
	}
	temp, err := os.CreateTemp(filepath.Dir(output), ".sandbox.gen.go.*")
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
	definitions := []source{{filepath.Join("packages", "contracts", "sandboxes.openapi.json"), openAPISHA256, nil}, {filepath.Join("packages", "contracts", "sandbox-template.schema.json"), templateSHA256, nil}, {filepath.Join("packages", "contracts", "sandbox-cli-contract.json"), cliSHA256, nil}}
	result := map[string]source{}
	for _, definition := range definitions {
		encoded, err := os.ReadFile(filepath.Join(root, definition.path))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(encoded)
		got := hex.EncodeToString(sum[:])
		if got != definition.digest {
			return nil, fmt.Errorf("%s digest %s is not represented by pinned sandbox client %s", definition.path, got, definition.digest)
		}
		if err = json.Unmarshal(encoded, &definition.document); err != nil {
			return nil, fmt.Errorf("decode %s: %w", definition.path, err)
		}
		result[definition.path] = definition
	}
	return result, nil
}

func validate(sources map[string]source, template string) error {
	api := sources[filepath.Join("packages", "contracts", "sandboxes.openapi.json")].document
	manifest := sources[filepath.Join("packages", "contracts", "sandbox-template.schema.json")].document
	cli := sources[filepath.Join("packages", "contracts", "sandbox-cli-contract.json")].document
	if atString(api, "openapi") != "3.1.0" || atString(api, "info", "version") != "v1alpha1" {
		return fmt.Errorf("sandbox API must be OpenAPI 3.1.0 v1alpha1")
	}
	if atString(api, "components", "securitySchemes", "bearerAuth", "bearerFormat") != "opaque" || atString(api, "components", "securitySchemes", "grantAuth", "name") != "Authorization" {
		return fmt.Errorf("sandbox authentication contract changed")
	}
	paths, _ := at(api, "paths").(map[string]any)
	if len(paths) != 16 {
		return fmt.Errorf("sandbox paths changed: got %d want 16", len(paths))
	}
	if err := validateOperations(api, template); err != nil {
		return err
	}
	if atString(manifest, "properties", "apiVersion", "const") != "blazn.dev/v1alpha1" || atString(manifest, "properties", "kind", "const") != "SandboxTemplate" {
		return fmt.Errorf("template identity changed")
	}
	if atString(manifest, "$defs", "spec", "properties", "policyProfile", "const") != "poc-restricted-v1" || atString(manifest, "$defs", "spec", "properties", "networkProfile", "const") != "default-deny-v1" || atString(manifest, "$defs", "spec", "properties", "isolation", "const") != "approved-non-sensitive-poc" {
		return fmt.Errorf("template POC policy changed")
	}
	if at(manifest, "x-blazn-invariants", "uniqueVariantArchitecture") != true || atString(manifest, "x-blazn-invariants", "canonicalization") != "RFC8785/JCS over the fully resolved spec object" {
		return fmt.Errorf("template identity invariants changed")
	}
	if err := validateExternalRefs(api); err != nil {
		return err
	}
	if atString(cli, "contractVersion") != "sandbox-cli/v1alpha1" {
		return fmt.Errorf("sandbox CLI contract version changed")
	}
	for _, forbidden := range []string{"accessToken", "grantToken", "bearerToken"} {
		if !containsString(at(cli, "security", "forbiddenArgv"), forbidden) || !containsString(at(cli, "security", "forbiddenOutput"), forbidden) {
			return fmt.Errorf("sandbox CLI secret exclusion changed for %s", forbidden)
		}
	}
	commands, _ := at(cli, "commands").(map[string]any)
	for _, command := range []string{"template validate", "template publish", "sandbox create", "sandbox list", "sandbox get", "sandbox watch", "sandbox exec", "sandbox upload", "sandbox download", "sandbox stop", "sandbox delete"} {
		if _, ok := commands[command]; !ok {
			return fmt.Errorf("CLI command missing: %s", command)
		}
	}
	if !requiredExactly(at(api, "components", "schemas", "Sandbox"), "id", "workspaceId", "requestedBy", "templateId", "templateVersionId", "templateName", "templateVersion", "templateDigest", "variantName", "imageIndexDigest", "imageDigest", "architecture", "allocationMode", "sourceBindings", "artifactContract", "state", "desiredState", "version", "queueName", "admissionId", "isolation", "expiresAt", "conditions", "createdAt", "updatedAt") {
		return fmt.Errorf("Sandbox required fields changed")
	}
	if !requiredExactly(at(api, "components", "schemas", "SandboxOperationReceipt"), "id", "operationId", "status", "cleanupComplete", "artifactExportComplete", "grantsRevoked", "backendDestroyed", "result", "error", "createdAt") || !requiredExactly(at(api, "components", "schemas", "SandboxEvent"), "eventId", "sandboxId", "operationId", "sequence", "type", "payload", "createdAt") || !requiredExactly(at(api, "components", "schemas", "SandboxArtifact"), "id", "workspaceId", "sandboxId", "name", "path", "mediaType", "size", "sha256", "exportedAt", "download") {
		return fmt.Errorf("typed operation/event/artifact required fields changed")
	}
	if !enumExactly(at(api, "components", "schemas", "CreateSandboxOperationRequest", "properties", "type", "enum"), "delete", "stop") || !enumExactly(at(api, "components", "schemas", "CreateAccessGrantRequest", "properties", "kind", "enum"), "download", "exec", "upload") {
		return fmt.Errorf("sandbox mutation enums changed")
	}
	for _, schemaName := range []string{"template", "templateVersion", "sandbox", "operation", "receipt", "event"} {
		if at(cli, "$defs", schemaName, "additionalProperties") != false {
			return fmt.Errorf("CLI schema %s must be closed", schemaName)
		}
	}
	for _, unsafe := range []string{`PathEscape(grantToken)`, `query.Set("token"`} {
		if strings.Contains(template, unsafe) {
			return fmt.Errorf("grant token enters unsafe transport surface: %q", unsafe)
		}
	}
	return nil
}

func validateExternalRefs(value any) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "$ref" {
				ref, _ := child.(string)
				if !strings.HasPrefix(ref, "#/") && ref != "sandbox-template.schema.json" {
					return fmt.Errorf("sandbox contract has unpinned external ref %q", ref)
				}
			}
			if err := validateExternalRefs(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := validateExternalRefs(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateOperations(api map[string]any, template string) error {
	seen := map[string]bool{}
	for _, expected := range sandboxOperations {
		operation, ok := at(api, "paths", expected.path, expected.method).(map[string]any)
		if !ok || atString(operation, "operationId") != expected.id {
			return fmt.Errorf("%s %s operationId changed", expected.method, expected.path)
		}
		seen[expected.path+" "+expected.method] = true
		if atString(operation, "responses", "default", "$ref") != "#/components/responses/SandboxError" {
			return fmt.Errorf("%s lacks common error", expected.id)
		}
		parameters, _ := operation["parameters"].([]any)
		if len(parameters) != len(expected.parameters) {
			return fmt.Errorf("%s parameters=%d want %d", expected.id, len(parameters), len(expected.parameters))
		}
		for i, want := range expected.parameters {
			if atString(parameters[i], "$ref") != "#/components/parameters/"+want {
				return fmt.Errorf("%s parameter %d changed", expected.id, i)
			}
		}
		body := at(operation, "requestBody")
		if expected.requestRef == "" {
			if body != nil {
				return fmt.Errorf("%s unexpectedly has request body", expected.id)
			}
		} else {
			if at(operation, "requestBody", "required") != true {
				return fmt.Errorf("%s body must be required", expected.id)
			}
			schema := at(operation, "requestBody", "content", expected.requestMedia, "schema")
			if expected.requestRef == "binary" {
				if atString(schema, "type") != "string" || atString(schema, "format") != "binary" {
					return fmt.Errorf("%s raw body changed", expected.id)
				}
			} else {
				want := "#/components/schemas/" + expected.requestRef
				if expected.requestRef == "sandbox-template.schema.json" {
					want = expected.requestRef
				}
				if atString(schema, "$ref") != want {
					return fmt.Errorf("%s request schema changed", expected.id)
				}
			}
		}
		responseSchema := at(operation, "responses", expected.success, "content", expected.responseMedia, "schema")
		if expected.responseRef == "binary" {
			if atString(responseSchema, "type") != "string" || atString(responseSchema, "format") != "binary" {
				return fmt.Errorf("%s raw response changed", expected.id)
			}
		} else if atString(responseSchema, "$ref") != "#/components/schemas/"+expected.responseRef {
			return fmt.Errorf("%s response schema changed", expected.id)
		}
		security := expected.security
		if security == "grant" {
			scopes, ok := at(operation, "security").([]any)
			if !ok || len(scopes) != 1 {
				return fmt.Errorf("%s grant security changed", expected.id)
			}
			entry, ok := scopes[0].(map[string]any)
			values, exists := entry["grantAuth"].([]any)
			if !ok || !exists || len(values) != 0 {
				return fmt.Errorf("%s grant security changed", expected.id)
			}
		} else if at(operation, "security") != nil {
			return fmt.Errorf("%s unexpectedly overrides bearer security", expected.id)
		}
		goName := strings.ToUpper(expected.id[:1]) + expected.id[1:]
		if strings.Count(template, "func (c *Client) "+goName+"(") != 1 {
			return fmt.Errorf("generated client method parity changed for %s", expected.id)
		}
	}
	paths, _ := at(api, "paths").(map[string]any)
	count := 0
	for path, pathValue := range paths {
		methods, _ := pathValue.(map[string]any)
		for method := range methods {
			count++
			if !seen[path+" "+method] {
				return fmt.Errorf("unrepresented sandbox operation %s %s", method, path)
			}
		}
	}
	if count != len(sandboxOperations) {
		return fmt.Errorf("sandbox operation count=%d want %d", count, len(sandboxOperations))
	}
	grantParams, _ := at(api, "paths", "/v1/sandboxes/{sandboxId}/access-grants", "post", "parameters").([]any)
	for _, parameter := range grantParams {
		if strings.HasSuffix(atString(parameter, "$ref"), "/IdempotencyKey") {
			return fmt.Errorf("one-time grant creation must not be idempotent")
		}
	}
	for _, header := range []string{"X-Content-Size", "X-Content-SHA256"} {
		if at(api, "paths", "/v1/sandbox-access-grants/{grantId}/file", "get", "responses", "200", "headers", header, "required") != true {
			return fmt.Errorf("grant download header %s must be required", header)
		}
	}
	return nil
}

func requiredExactly(schema any, want ...string) bool {
	values, _ := at(schema, "required").([]any)
	got := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			got = append(got, text)
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	return strings.Join(got, "\x00") == strings.Join(want, "\x00")
}
func enumExactly(value any, want ...string) bool {
	values, _ := value.([]any)
	got := make([]string, 0, len(values))
	for _, item := range values {
		if text, ok := item.(string); ok {
			got = append(got, text)
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	return strings.Join(got, "\x00") == strings.Join(want, "\x00")
}

func containsString(value any, want string) bool {
	items, _ := value.([]any)
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func errorStatusSource(api map[string]any) string {
	statuses, _ := at(api, "components", "schemas", "SandboxError", "x-blazn-error-status").(map[string]any)
	keys := make([]string, 0, len(statuses))
	for key := range statuses {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&b, "\n\t%q: %s,", key, strconv.Itoa(int(statuses[key].(float64))))
	}
	return b.String()
}
func at(value any, path ...string) any {
	current := value
	for _, part := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[part]
	}
	return current
}
func atString(value any, path ...string) string {
	result, _ := at(value, path...).(string)
	return result
}
func repositoryRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("locate generator")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..")), nil
}
func fatalIf(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

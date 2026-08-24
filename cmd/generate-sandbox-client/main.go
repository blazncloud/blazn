package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
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
	openAPISHA256  = "c6170596484d7e01ec45faf9d185460b3f8aa90f79d09b67c8ae2f902e0cba02"
	templateSHA256 = "976016e40e20203be6309a09356d7fdf1d21c16a3ec676e478e6f8e5a758ebb6"
	cliSHA256      = "e40063e5f7b1edc107282a637e3d67f1d477467c8e9243d1ae082c0a44c3da83"
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
	fatalIf(validateGeneratedGo(generated, openapi))
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
	if !requiredExactly(at(api, "components", "schemas", "SandboxOperationReceipt"), "id", "operationId", "operationType", "status", "cleanupComplete", "artifactExportComplete", "grantsRevoked", "backendDestroyed", "backend", "result", "error", "createdAt") || !requiredExactly(at(api, "components", "schemas", "SandboxEvent"), "eventId", "sandboxId", "operationId", "sequence", "type", "payload", "createdAt") || !requiredExactly(at(api, "components", "schemas", "SandboxArtifact"), "id", "workspaceId", "sandboxId", "name", "path", "mediaType", "size", "sha256", "exportedAt", "download") {
		return fmt.Errorf("typed operation/event/artifact required fields changed")
	}
	if !requiredExactly(at(api, "components", "schemas", "CreateSandboxRequest"), "template", "architecture", "allocationMode", "expiresInSeconds", "sources", "approvedNonSensitive") || !requiredExactly(at(api, "components", "schemas", "CreateSandboxOperationRequest"), "type", "expectedVersion") || !requiredExactly(at(api, "components", "schemas", "CreateAccessGrantRequest"), "kind", "expiresInSeconds") {
		return fmt.Errorf("sandbox request required fields changed")
	}
	if !enumExactly(at(api, "components", "schemas", "CreateSandboxOperationRequest", "properties", "type", "enum"), "delete", "stop") || !enumExactly(at(api, "components", "schemas", "CreateAccessGrantRequest", "properties", "kind", "enum"), "download", "exec", "upload") {
		return fmt.Errorf("sandbox mutation enums changed")
	}
	for schemaName, apiName := range map[string]string{"template": "SandboxTemplate", "templateVersion": "SandboxTemplateVersion", "sandbox": "Sandbox", "operation": "SandboxOperation", "receipt": "SandboxOperationReceipt", "event": "SandboxEvent"} {
		if atString(cli, "$defs", schemaName, "$ref") != "https://blazn.dev/contracts/sandboxes.openapi.json#/components/schemas/"+apiName {
			return fmt.Errorf("CLI schema %s must reference exact API schema", schemaName)
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

var generatedMethodSignatures = map[string]string{
	"createSandboxTemplate":         "(context.Context,string,string,string,SandboxManifest)(SandboxTemplateEnvelope,error)",
	"listSandboxTemplates":          "(context.Context,string,string,string)(SandboxTemplateList,error)",
	"getSandboxTemplate":            "(context.Context,string,string)(SandboxTemplateEnvelope,error)",
	"replaceSandboxTemplateDraft":   "(context.Context,string,string,string,ReplaceSandboxTemplateDraftRequest)(SandboxTemplateEnvelope,error)",
	"publishSandboxTemplateVersion": "(context.Context,string,string,string,PublishSandboxTemplateVersionRequest)(SandboxTemplateVersionEnvelope,error)",
	"listSandboxTemplateVersions":   "(context.Context,string,string,string)(SandboxTemplateVersionList,error)",
	"getSandboxTemplateVersion":     "(context.Context,string,string)(SandboxTemplateVersionEnvelope,error)",
	"createSandbox":                 "(context.Context,string,string,string,CreateSandboxRequest)(SandboxMutation,error)",
	"listSandboxes":                 "(context.Context,string,string,string)(SandboxList,error)",
	"getSandbox":                    "(context.Context,string,string)(Sandbox,error)",
	"streamSandboxEvents":           "(context.Context,string,string,string)(*SandboxEventStream,error)",
	"createSandboxOperation":        "(context.Context,string,string,string,CreateSandboxOperationRequest)(SandboxMutation,error)",
	"createSandboxAccessGrant":      "(context.Context,string,string,CreateSandboxAccessGrantRequest)(SandboxAccessGrantCreated,error)",
	"executeSandboxGrant":           "(context.Context,string,string,SandboxExecRequest)(SandboxExecResult,error)",
	"uploadSandboxGrantFile":        "(context.Context,string,string,string,string,io.Reader,int64)(SandboxFileTransferResult,error)",
	"downloadSandboxGrantFile":      "(context.Context,string,string,string)(io.ReadCloser,int64,string,error)",
	"getSandboxOperation":           "(context.Context,string,string)(SandboxOperation,error)",
	"listSandboxArtifacts":          "(context.Context,string,string,string)(SandboxArtifactList,error)",
	"getSandboxArtifact":            "(context.Context,string,string)(SandboxArtifact,error)",
	"downloadSandboxArtifact":       "(context.Context,string,string)(io.ReadCloser,int64,string,error)",
}

func validateGeneratedGo(source []byte, api map[string]any) error {
	fileset := token.NewFileSet()
	file, err := parser.ParseFile(fileset, "sandbox.gen.go", source, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("parse generated sandbox client: %w", err)
	}
	methods := map[string]string{}
	methodBodies := map[string]string{}
	structs := map[string][]string{}
	constants := map[string][]string{}
	for _, declaration := range file.Decls {
		switch typed := declaration.(type) {
		case *ast.FuncDecl:
			if typed.Recv != nil {
				methods[typed.Name.Name] = goFunctionSignature(fileset, typed.Type)
				var rendered bytes.Buffer
				_ = printer.Fprint(&rendered, fileset, typed)
				methodBodies[typed.Name.Name] = rendered.String()
			}
		case *ast.GenDecl:
			if typed.Tok == token.CONST {
				for _, specification := range typed.Specs {
					valueSpec, ok := specification.(*ast.ValueSpec)
					if !ok || valueSpec.Type == nil {
						continue
					}
					typeName := goExpression(fileset, valueSpec.Type)
					for _, value := range valueSpec.Values {
						literal, ok := value.(*ast.BasicLit)
						if !ok || literal.Kind != token.STRING {
							continue
						}
						decoded, err := strconv.Unquote(literal.Value)
						if err == nil {
							constants[typeName] = append(constants[typeName], decoded)
						}
					}
				}
				continue
			}
			if typed.Tok != token.TYPE {
				continue
			}
			for _, specification := range typed.Specs {
				typeSpec, ok := specification.(*ast.TypeSpec)
				if !ok {
					continue
				}
				structure, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, field := range structure.Fields.List {
					if field.Tag == nil {
						continue
					}
					var tag string
					if _, err := fmt.Sscanf(field.Tag.Value, "`json:%q`", &tag); err != nil {
						continue
					}
					tag = strings.Split(tag, ",")[0]
					if tag != "" && tag != "-" {
						structs[typeSpec.Name.Name] = append(structs[typeSpec.Name.Name], tag)
					}
				}
			}
		}
	}
	for _, operation := range sandboxOperations {
		name := strings.ToUpper(operation.id[:1]) + operation.id[1:]
		want := generatedMethodSignatures[operation.id]
		if want == "" || methods[name] != want {
			return fmt.Errorf("generated Go signature %s=%q want %q", name, methods[name], want)
		}
		body := methodBodies[name]
		for _, fragment := range openAPIPathFragments(operation.path) {
			if !strings.Contains(body, strconv.Quote(fragment)) {
				return fmt.Errorf("generated Go method %s lacks path fragment %q", name, fragment)
			}
		}
		statusName := map[string]string{"200": "http.StatusOK", "201": "http.StatusCreated", "202": "http.StatusAccepted"}[operation.success]
		if operation.id != "executeSandboxGrant" && !strings.Contains(body, statusName) {
			return fmt.Errorf("generated Go method %s lacks success status %s", name, statusName)
		}
	}
	bindings := map[string]string{"Sandbox": "Sandbox", "SandboxOperation": "SandboxOperation", "SandboxOperationReceipt": "SandboxOperationReceipt", "SandboxEvent": "SandboxEvent", "SandboxArtifact": "SandboxArtifact", "CreateSandboxRequest": "CreateSandboxRequest", "CreateSandboxOperationRequest": "CreateSandboxOperationRequest", "CreateAccessGrantRequest": "CreateSandboxAccessGrantRequest"}
	for schemaName, goName := range bindings {
		properties, _ := at(api, "components", "schemas", schemaName, "properties").(map[string]any)
		want := make([]string, 0, len(properties))
		for name := range properties {
			want = append(want, name)
		}
		got := append([]string(nil), structs[goName]...)
		sort.Strings(want)
		sort.Strings(got)
		if strings.Join(want, "\x00") != strings.Join(got, "\x00") {
			return fmt.Errorf("generated Go fields %s=%v want %v", goName, got, want)
		}
	}
	enumBindings := map[string]any{
		"SandboxArchitecture":    at(api, "components", "schemas", "CreateSandboxRequest", "properties", "architecture", "enum"),
		"SandboxAllocationMode":  at(api, "components", "schemas", "CreateSandboxRequest", "properties", "allocationMode", "enum"),
		"SandboxState":           at(api, "components", "schemas", "Sandbox", "properties", "state", "enum"),
		"SandboxDesiredState":    at(api, "components", "schemas", "Sandbox", "properties", "desiredState", "enum"),
		"SandboxOperationType":   at(api, "components", "schemas", "SandboxOperation", "properties", "type", "enum"),
		"SandboxOperationStatus": at(api, "components", "schemas", "SandboxOperation", "properties", "status", "enum"),
		"SandboxGrantKind":       at(api, "components", "schemas", "AccessGrant", "properties", "kind", "enum"),
		"SandboxGrantState":      at(api, "components", "schemas", "AccessGrant", "properties", "state", "enum"),
	}
	for typeName, enum := range enumBindings {
		items, _ := enum.([]any)
		want := make([]string, 0, len(items))
		for _, item := range items {
			if value, ok := item.(string); ok {
				want = append(want, value)
			}
		}
		got := append([]string(nil), constants[typeName]...)
		sort.Strings(want)
		sort.Strings(got)
		if strings.Join(want, "\x00") != strings.Join(got, "\x00") {
			return fmt.Errorf("generated Go enum %s=%v want %v", typeName, got, want)
		}
	}
	compact := strings.NewReplacer(" ", "", "\n", "", "\t", "").Replace(string(source))
	for _, required := range []string{`Header.Set("Authorization","Bearer"+accessToken)`, `Header.Set("Authorization","Blazn-Grant"+grantToken)`, `Header.Set("Idempotency-Key",idempotencyKey)`, `Header.Set("Last-Event-ID",lastEventID)`, `Header.Set("X-Blazn-Sandbox-Path",sandboxPath)`, `Header.Set("X-Content-Size",strconv.FormatInt(size,10))`, `Header.Set("X-Content-SHA256",digest)`, `Header.Set("Content-Type","application/octet-stream")`, `Header.Set("Content-Type","application/json")`, `Header.Set("Accept","text/event-stream")`} {
		if !strings.Contains(compact, required) {
			return fmt.Errorf("generated Go transport parity lacks %s", required)
		}
	}
	return nil
}

func openAPIPathFragments(path string) []string {
	parts := []string{}
	for len(path) > 0 {
		open := strings.IndexByte(path, '{')
		if open < 0 {
			if path != "" {
				parts = append(parts, path)
			}
			break
		}
		if open > 0 {
			parts = append(parts, path[:open])
		}
		close := strings.IndexByte(path[open:], '}')
		if close < 0 {
			break
		}
		path = path[open+close+1:]
	}
	return parts
}

func goFunctionSignature(fileset *token.FileSet, function *ast.FuncType) string {
	render := func(expression ast.Expr) string { return goExpression(fileset, expression) }
	collect := func(fields *ast.FieldList) []string {
		if fields == nil {
			return nil
		}
		values := []string{}
		for _, field := range fields.List {
			count := len(field.Names)
			if count == 0 {
				count = 1
			}
			for range count {
				values = append(values, render(field.Type))
			}
		}
		return values
	}
	return "(" + strings.Join(collect(function.Params), ",") + ")(" + strings.Join(collect(function.Results), ",") + ")"
}

func goExpression(fileset *token.FileSet, expression ast.Expr) string {
	var output bytes.Buffer
	_ = printer.Fprint(&output, fileset, expression)
	return output.String()
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

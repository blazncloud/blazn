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
	"sort"
	"strings"
)

//go:embed workspace.gen.go.tmpl
var workspaceTemplate []byte

const supportedWorkspaceContractSHA256 = "3f792738396e1af07dd9e6418b5192529fbdd82ebce0fda33a3d16c60ac90be9"

type operation struct {
	path, method, id string
	parameters       []string
	requestRef       string
	status           string
	responseRef      string
	responseType     string
}

var operations = []operation{
	{"/v1/workspaces", "post", "createWorkspace", []string{"IdempotencyKey"}, "CreateWorkspaceRequest", "201", "WorkspaceEnvelope", "application/json"},
	{"/v1/workspaces", "get", "listWorkspaces", []string{"Cursor"}, "", "200", "WorkspaceList", "application/json"},
	{"/v1/workspaces/{workspaceId}", "get", "getWorkspace", []string{"WorkspaceId"}, "", "200", "WorkspaceEnvelope", "application/json"},
	{"/v1/workspaces/{workspaceId}", "patch", "updateWorkspace", []string{"WorkspaceId", "IdempotencyKey"}, "UpdateWorkspaceRequest", "200", "WorkspaceEnvelope", "application/json"},
	{"/v1/workspaces/{workspaceId}/invitations", "post", "createWorkspaceInvitation", []string{"WorkspaceId", "IdempotencyKey"}, "CreateInvitationRequest", "201", "InvitationCreated", "application/json"},
	{"/v1/workspaces/{workspaceId}/invitations", "get", "listWorkspaceInvitations", []string{"WorkspaceId", "Cursor"}, "", "200", "InvitationList", "application/json"},
	{"/v1/workspaces/{workspaceId}/invitations/{invitationId}", "delete", "revokeWorkspaceInvitation", []string{"WorkspaceId", "InvitationId", "ExpectedVersion", "IdempotencyKey"}, "", "200", "MutationResult", "application/json"},
	{"/v1/workspace-invitations/accept", "post", "acceptWorkspaceInvitation", []string{"IdempotencyKey"}, "AcceptInvitationRequest", "200", "WorkspaceEnvelope", "application/json"},
	{"/v1/workspaces/{workspaceId}/members", "get", "listWorkspaceMembers", []string{"WorkspaceId", "Cursor"}, "", "200", "MembershipList", "application/json"},
	{"/v1/workspaces/{workspaceId}/members/{userId}", "patch", "updateWorkspaceMember", []string{"WorkspaceId", "UserId", "IdempotencyKey"}, "UpdateMembershipRequest", "200", "Membership", "application/json"},
	{"/v1/workspaces/{workspaceId}/members/{userId}", "delete", "removeWorkspaceMember", []string{"WorkspaceId", "UserId", "ExpectedVersion", "IdempotencyKey"}, "", "200", "MutationResult", "application/json"},
	{"/v1/workspaces/{workspaceId}/membership", "delete", "leaveWorkspace", []string{"WorkspaceId", "ExpectedVersion", "IdempotencyKey"}, "", "200", "MutationResult", "application/json"},
	{"/v1/workspaces/{workspaceId}/events", "get", "streamWorkspaceEvents", []string{"WorkspaceId", "Last-Event-ID"}, "", "200", "", "text/event-stream"},
}

var schemaFields = map[string][]string{
	"WorkspaceError":          {"code", "message", "requestId"},
	"Workspace":               {"createdAt", "currentUserRole", "id", "name", "slug", "status", "updatedAt", "version"},
	"WorkspaceEnvelope":       {"workspace"},
	"WorkspaceList":           {"items", "nextCursor"},
	"CreateWorkspaceRequest":  {"name", "slug"},
	"UpdateWorkspaceRequest":  {"expectedVersion", "name"},
	"Invitation":              {"createdAt", "expiresAt", "id", "role", "status", "version", "workspaceId"},
	"InvitationCreated":       {"invitation", "inviteToken"},
	"InvitationList":          {"items", "nextCursor"},
	"CreateInvitationRequest": {"expiresIn", "role"},
	"AcceptInvitationRequest": {"inviteToken"},
	"Membership":              {"joinedAt", "removedAt", "role", "status", "user", "version", "workspaceId"},
	"MembershipList":          {"items", "nextCursor"},
	"UpdateMembershipRequest": {"expectedVersion", "role"},
	"MutationResult":          {"invitationId", "status", "userId", "version", "workspaceId"},
}

var schemaRequired = map[string][]string{
	"WorkspaceError":          {"code", "message", "requestId"},
	"Workspace":               {"createdAt", "currentUserRole", "id", "name", "slug", "status", "updatedAt", "version"},
	"WorkspaceEnvelope":       {"workspace"},
	"WorkspaceList":           {"items", "nextCursor"},
	"CreateWorkspaceRequest":  {"name"},
	"UpdateWorkspaceRequest":  {"expectedVersion", "name"},
	"Invitation":              {"createdAt", "expiresAt", "id", "role", "status", "version", "workspaceId"},
	"InvitationCreated":       {"invitation", "inviteToken"},
	"InvitationList":          {"items", "nextCursor"},
	"CreateInvitationRequest": {"expiresIn", "role"},
	"AcceptInvitationRequest": {"inviteToken"},
	"Membership":              {"joinedAt", "role", "status", "user", "version", "workspaceId"},
	"MembershipList":          {"items", "nextCursor"},
	"UpdateMembershipRequest": {"expectedVersion", "role"},
	"MutationResult":          {"status", "version", "workspaceId"},
}

func main() {
	check := flag.Bool("check", false, "fail if the checked-in workspace client differs")
	flag.Parse()
	root, err := repositoryRoot()
	fatalIf(err)
	contractPath := filepath.Join(root, "packages", "contracts", "workspaces.openapi.json")
	outputPath := filepath.Join(root, "internal", "client", "workspace.gen.go")
	contract, err := os.ReadFile(contractPath)
	fatalIf(err)
	digest := sha256.Sum256(contract)
	digestString := hex.EncodeToString(digest[:])
	if digestString != supportedWorkspaceContractSHA256 {
		fatalIf(fmt.Errorf("workspace OpenAPI contract %s is not represented by pinned client %s", digestString, supportedWorkspaceContractSHA256))
	}
	var document map[string]any
	fatalIf(json.Unmarshal(contract, &document))
	fatalIf(validate(document, string(workspaceTemplate)))
	generated := render(digestString)
	if *check {
		current, err := os.ReadFile(outputPath)
		fatalIf(err)
		if !bytes.Equal(current, generated) {
			fatalIf(fmt.Errorf("generated workspace client is stale; run 'make generate-workspace-client'"))
		}
		return
	}
	temp, err := os.CreateTemp(filepath.Dir(outputPath), ".workspace.gen.go.*")
	fatalIf(err)
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(generated); err != nil {
		_ = temp.Close()
		fatalIf(err)
	}
	fatalIf(temp.Sync())
	fatalIf(temp.Close())
	fatalIf(os.Rename(tempPath, outputPath))
}

func validate(document map[string]any, template string) error {
	if atString(document, "openapi") != "3.1.0" || atString(document, "info", "version") != "v1alpha1" {
		return fmt.Errorf("workspace overlay must be OpenAPI 3.1.0 v1alpha1")
	}
	if atString(document, "components", "securitySchemes", "bearerAuth", "bearerFormat") != "opaque" {
		return fmt.Errorf("workspace bearer authentication must be opaque")
	}
	security, ok := at(document, "security").([]any)
	if !ok || len(security) != 1 {
		return fmt.Errorf("workspace overlay must require global bearerAuth")
	}
	scopes, exists := at(security[0], "bearerAuth").([]any)
	if !exists || len(scopes) != 0 {
		return fmt.Errorf("workspace overlay bearerAuth scopes changed")
	}
	paths, ok := at(document, "paths").(map[string]any)
	if !ok || len(paths) != 9 {
		return fmt.Errorf("workspace overlay paths changed: got %d want 9", len(paths))
	}
	expectedMethods := make(map[string][]string)
	for _, expected := range operations {
		expectedMethods[expected.path] = append(expectedMethods[expected.path], expected.method)
		base := []string{"paths", expected.path, expected.method}
		if got := atString(document, append(base, "operationId")...); got != expected.id {
			return fmt.Errorf("%s %s operationId=%q want %q", expected.method, expected.path, got, expected.id)
		}
		if err := validateParameters(document, base, expected.parameters); err != nil {
			return fmt.Errorf("%s: %w", expected.id, err)
		}
		if expected.requestRef != "" {
			if got := atString(document, append(base, "requestBody", "content", "application/json", "schema", "$ref")...); got != schemaRef(expected.requestRef) {
				return fmt.Errorf("%s request schema=%q", expected.id, got)
			}
		} else if at(document, append(base, "requestBody")...) != nil {
			return fmt.Errorf("%s unexpectedly has a request body", expected.id)
		}
		responseBase := append(base, "responses", expected.status, "content", expected.responseType, "schema")
		if expected.responseRef != "" {
			if got := atString(document, append(responseBase, "$ref")...); got != schemaRef(expected.responseRef) {
				return fmt.Errorf("%s response schema=%q", expected.id, got)
			}
		} else if atString(document, append(responseBase, "type")...) != "string" {
			return fmt.Errorf("%s SSE response schema changed", expected.id)
		}
		if atString(document, append(base, "responses", "default", "$ref")...) != "#/components/responses/WorkspaceError" {
			return fmt.Errorf("%s default error response changed", expected.id)
		}
	}
	for path, value := range paths {
		methods, _ := value.(map[string]any)
		want := append([]string(nil), expectedMethods[path]...)
		got := make([]string, 0, len(methods))
		for method := range methods {
			got = append(got, method)
		}
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			return fmt.Errorf("path %s methods=%v want %v", path, got, want)
		}
	}
	if err := validateComponentParameters(document); err != nil {
		return err
	}
	schemas, _ := at(document, "components", "schemas").(map[string]any)
	if len(schemas) != len(schemaFields)+1 {
		return fmt.Errorf("workspace schemas changed: got %d want %d", len(schemas), len(schemaFields)+1)
	}
	roleEnum, _ := at(document, "components", "schemas", "Role", "enum").([]any)
	if fmt.Sprint(roleEnum) != "[owner administrator operator member viewer]" {
		return fmt.Errorf("Role enum changed: %v", roleEnum)
	}
	for schema, fields := range schemaFields {
		object, ok := at(document, "components", "schemas", schema).(map[string]any)
		if !ok || object["type"] != "object" || object["additionalProperties"] != false {
			return fmt.Errorf("schema %s must be a closed object", schema)
		}
		properties, ok := object["properties"].(map[string]any)
		if !ok {
			return fmt.Errorf("schema %s properties missing", schema)
		}
		got := sortedKeys(properties)
		if strings.Join(got, ",") != strings.Join(fields, ",") {
			return fmt.Errorf("schema %s fields=%v want %v", schema, got, fields)
		}
		required, _ := object["required"].([]any)
		requiredNames := make([]string, 0, len(required))
		for _, value := range required {
			name, ok := value.(string)
			if !ok {
				return fmt.Errorf("schema %s has a non-string required field", schema)
			}
			requiredNames = append(requiredNames, name)
		}
		sort.Strings(requiredNames)
		if strings.Join(requiredNames, ",") != strings.Join(schemaRequired[schema], ",") {
			return fmt.Errorf("schema %s required=%v want %v", schema, requiredNames, schemaRequired[schema])
		}
		for _, field := range got {
			if !strings.Contains(template, `json:"`+field) {
				return fmt.Errorf("workspace template lacks %s.%s", schema, field)
			}
		}
	}
	if atString(document, "components", "schemas", "Membership", "properties", "user", "$ref") != "./openapi.json#/components/schemas/User" {
		return fmt.Errorf("Membership.user must reuse auth User schema")
	}
	if !strings.Contains(template, `Header.Set("Authorization", "Bearer "+accessToken)`) || !strings.Contains(template, `Header.Set("Idempotency-Key", idempotencyKey)`) || !strings.Contains(template, `Header.Set("Last-Event-ID", lastEventID)`) {
		return fmt.Errorf("workspace template is missing required headers")
	}
	if strings.Contains(template, "PathEscape(request.InviteToken)") || strings.Contains(template, `query.Set("inviteToken"`) {
		return fmt.Errorf("invitation token must never enter URL construction")
	}
	return nil
}

func validateParameters(document map[string]any, base []string, expected []string) error {
	values, _ := at(document, append(base, "parameters")...).([]any)
	if len(values) != len(expected) {
		return fmt.Errorf("parameters=%d want %d", len(values), len(expected))
	}
	for index, name := range expected {
		object, _ := values[index].(map[string]any)
		if name == "Last-Event-ID" {
			if object["name"] != name || object["in"] != "header" || object["required"] != false || atString(object, "schema", "type") != "string" {
				return fmt.Errorf("Last-Event-ID header changed")
			}
			continue
		}
		if object["$ref"] != "#/components/parameters/"+name {
			return fmt.Errorf("parameter %d=%v want %s", index, object["$ref"], name)
		}
	}
	return nil
}

func validateComponentParameters(document map[string]any) error {
	expected := map[string][3]string{
		"WorkspaceId":     {"workspaceId", "path", "uuid"},
		"InvitationId":    {"invitationId", "path", "uuid"},
		"UserId":          {"userId", "path", "uuid"},
		"IdempotencyKey":  {"Idempotency-Key", "header", "string"},
		"ExpectedVersion": {"expectedVersion", "query", "integer"},
		"Cursor":          {"cursor", "query", "string"},
	}
	parameters, _ := at(document, "components", "parameters").(map[string]any)
	if len(parameters) != len(expected) {
		return fmt.Errorf("component parameters changed")
	}
	for key, want := range expected {
		object, _ := parameters[key].(map[string]any)
		if object["name"] != want[0] || object["in"] != want[1] || atString(object, "schema", "type") != want[2] && atString(object, "schema", "format") != want[2] {
			return fmt.Errorf("component parameter %s changed", key)
		}
	}
	return nil
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func atString(root any, path ...string) string {
	value, _ := at(root, path...).(string)
	return value
}

func at(root any, path ...string) any {
	current := root
	for _, part := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[part]
	}
	return current
}

func schemaRef(name string) string { return "#/components/schemas/" + name }

func render(digest string) []byte {
	_, rest, found := bytes.Cut(workspaceTemplate, []byte("\n"))
	if !found {
		rest = workspaceTemplate
	}
	header := "// Code generated from packages/contracts/workspaces.openapi.json; DO NOT EDIT.\n// Contract SHA256: " + digest + "\n"
	return append([]byte(header), rest...)
}

func repositoryRoot() (string, error) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("locate generator source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return "", fmt.Errorf("locate repository root: %w", err)
	}
	return root, nil
}

func fatalIf(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate-workspace-client:", err)
		os.Exit(1)
	}
}

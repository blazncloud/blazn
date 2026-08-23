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

//go:embed project.gen.go.tmpl
var projectTemplate []byte

const supportedProjectContractSHA256 = "32000a7b33ee03d8d81085ffe6b76c3e43c556285a771f06dbeac3c0b1e07a45"

var operations = map[string]string{
	"POST /v1/workspaces/{workspaceId}/projects":              "createProject",
	"GET /v1/workspaces/{workspaceId}/projects":               "listProjects",
	"GET /v1/workspaces/{workspaceId}/projects/{projectId}":   "getProject",
	"PATCH /v1/workspaces/{workspaceId}/projects/{projectId}": "updateProject",
	"GET /v1/workspaces/{workspaceId}/projects/{projectId}/profiles/{profileKind}": "getProjectProfile",
	"PUT /v1/workspaces/{workspaceId}/projects/{projectId}/profiles/{profileKind}": "putProjectProfile",
}

var schemaFields = map[string][]string{
	"Project":              {"createdAt", "createdBy", "description", "id", "kind", "name", "slug", "status", "updatedAt", "version", "workspaceId"},
	"ProjectEnvelope":      {"project"},
	"ProjectList":          {"items", "nextCursor"},
	"CreateProjectRequest": {"description", "kind", "name", "slug"},
	"UpdateProjectRequest": {"description", "expectedVersion", "name", "status"},
	"ProjectProfile": {"artifactId", "createdAt", "createdBy", "digest", "draftId", "kind", "projectId", "schemaVersion", "status", "updatedAt", "updatedBy", "version", "workspaceId"},
	"ProjectProfileEnvelope": {"profile"}, "PutProjectProfileRequest": {"artifactId", "digest", "draftId", "expectedVersion", "schemaVersion", "status"},
	"ProjectError":         {"code", "message", "requestId"},
}

var schemaRequired = map[string][]string{
	"Project":              {"createdAt", "createdBy", "description", "id", "kind", "name", "slug", "status", "updatedAt", "version", "workspaceId"},
	"ProjectEnvelope":      {"project"},
	"ProjectList":          {"items", "nextCursor"},
	"CreateProjectRequest": {"name"},
	"UpdateProjectRequest": {"expectedVersion"},
	"ProjectProfile": {"artifactId", "createdAt", "createdBy", "digest", "draftId", "kind", "projectId", "schemaVersion", "status", "updatedAt", "updatedBy", "version", "workspaceId"},
	"ProjectProfileEnvelope": {"profile"}, "PutProjectProfileRequest": {"artifactId", "digest", "draftId", "expectedVersion", "schemaVersion", "status"},
	"ProjectError":         {"code", "message", "requestId"},
}

func main() {
	check := flag.Bool("check", false, "fail if the checked-in Project client differs")
	flag.Parse()
	root, err := repositoryRoot()
	fatalIf(err)
	contractPath := filepath.Join(root, "packages", "contracts", "projects.openapi.json")
	outputPath := filepath.Join(root, "internal", "client", "project.gen.go")
	contract, err := os.ReadFile(contractPath)
	fatalIf(err)
	digest := sha256.Sum256(contract)
	digestString := hex.EncodeToString(digest[:])
	if digestString != supportedProjectContractSHA256 {
		fatalIf(fmt.Errorf("Project OpenAPI contract %s is not represented by pinned client %s", digestString, supportedProjectContractSHA256))
	}
	var document map[string]any
	fatalIf(json.Unmarshal(contract, &document))
	fatalIf(validate(document, string(projectTemplate)))
	generated, err := format.Source(bytes.ReplaceAll(projectTemplate, []byte("{{CONTRACT_SHA256}}"), []byte(digestString)))
	fatalIf(err)
	if *check {
		current, err := os.ReadFile(outputPath)
		fatalIf(err)
		if !bytes.Equal(current, generated) {
			fatalIf(fmt.Errorf("generated Project client is stale; run 'make generate-project-client'"))
		}
		return
	}
	temp, err := os.CreateTemp(filepath.Dir(outputPath), ".project.gen.go.*")
	fatalIf(err)
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	_, err = temp.Write(generated)
	fatalIf(err)
	fatalIf(temp.Sync())
	fatalIf(temp.Close())
	fatalIf(os.Rename(tempPath, outputPath))
}

func validate(document map[string]any, template string) error {
	if stringAt(document, "openapi") != "3.1.0" || stringAt(document, "info", "version") != "v1alpha1" {
		return fmt.Errorf("Project contract version changed")
	}
	if stringAt(document, "servers", "0", "url") != "https://blazn.benpelo.com" {
		return fmt.Errorf("Project server origin changed")
	}
	paths, ok := valueAt(document, "paths").(map[string]any)
	if !ok || len(paths) != 3 {
		return fmt.Errorf("Project paths changed")
	}
	seen := map[string]string{}
	for path, raw := range paths {
		methods, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("Project path %s is invalid", path)
		}
		for method, operation := range methods {
			seen[strings.ToUpper(method)+" "+path] = stringAt(operation, "operationId")
		}
	}
	if fmt.Sprint(seen) == "" || len(seen) != len(operations) {
		return fmt.Errorf("Project operations changed")
	}
	for key, operationID := range operations {
		if seen[key] != operationID {
			return fmt.Errorf("Project operation %s=%q want %q", key, seen[key], operationID)
		}
		method := strings.ToLower(strings.SplitN(key, " ", 2)[0])
		path := strings.SplitN(key, " ", 2)[1]
		if stringAt(paths, path, method, "responses", "default", "$ref") != "#/components/responses/ProjectError" {
			return fmt.Errorf("Project operation %s lost its default error", operationID)
		}
	}
	schemas, ok := valueAt(document, "components", "schemas").(map[string]any)
	if !ok {
		return fmt.Errorf("Project schemas are missing")
	}
	for name, fields := range schemaFields {
		schema, ok := schemas[name].(map[string]any)
		if !ok || schema["additionalProperties"] != false {
			return fmt.Errorf("Project schema %s must be closed", name)
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			return fmt.Errorf("Project schema %s properties are missing", name)
		}
		got := keys(properties)
		if strings.Join(got, ",") != strings.Join(fields, ",") {
			return fmt.Errorf("Project schema %s fields=%v want %v", name, got, fields)
		}
		required, _ := schema["required"].([]any)
		requiredNames := make([]string, 0, len(required))
		for _, value := range required {
			field, ok := value.(string)
			if !ok {
				return fmt.Errorf("Project schema %s has a non-string required field", name)
			}
			requiredNames = append(requiredNames, field)
		}
		sort.Strings(requiredNames)
		if strings.Join(requiredNames, ",") != strings.Join(schemaRequired[name], ",") {
			return fmt.Errorf("Project schema %s required=%v want %v", name, requiredNames, schemaRequired[name])
		}
	}
	for _,name:=range []string{"ProjectStatus","ProjectProfileStatus"}{got:=stringSliceAt(schemas,name,"enum");sort.Strings(got);if strings.Join(got,",")!="active,archived"{return fmt.Errorf("Project schema %s enum changed",name)}}
	for _, marker := range []string{"func (c *Client) CreateProject", "func (c *Client) ListProjects", "func (c *Client) GetProject", "func (c *Client) UpdateProject", "func (c *Client) GetProjectProfile", "func (c *Client) PutProjectProfile", "type ProjectError = ErrorBody"} {
		if !strings.Contains(template, marker) {
			return fmt.Errorf("Project template lacks %s", marker)
		}
	}
	return nil
}

func valueAt(value any, path ...string) any {
	current := value
	for _, part := range path {
		if object, ok := current.(map[string]any); ok {
			current = object[part]
			continue
		}
		if array, ok := current.([]any); ok {
			var index int
			if _, err := fmt.Sscan(part, &index); err != nil || index < 0 || index >= len(array) {
				return nil
			}
			current = array[index]
			continue
		}
		return nil
	}
	return current
}

func stringAt(value any, path ...string) string {
	result, _ := valueAt(value, path...).(string)
	return result
}
func stringSliceAt(value any,path ...string)[]string{values,_:=valueAt(value,path...).([]any);result:=make([]string,0,len(values));for _,value:=range values{if text,ok:=value.(string);ok{result=append(result,text)}};return result}

func keys(values map[string]any) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func repositoryRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("resolve generator path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..")), nil
}

func fatalIf(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

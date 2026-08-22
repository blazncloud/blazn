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

//go:embed client.gen.go.tmpl
var clientTemplate []byte

// supportedContractSHA256 pins the complete schema semantics represented by
// clientTemplate. A contract change therefore cannot be accepted by merely
// rerunning the generator; the validator and typed template must be reviewed
// before this fingerprint is deliberately updated.
const supportedContractSHA256 = "9e36d86d26ce60d7fe7b34612c4af5548f62202c2cfbd56af669d4e6dcf6d0c7"

type operation struct {
	path        string
	method      string
	id          string
	requestRef  string
	status      string
	responseRef string
}

var operations = []operation{
	{"/v1/auth/device/authorizations", "post", "createDeviceAuthorization", "DeviceAuthorizationRequest", "201", "DeviceAuthorization"},
	{"/v1/auth/device/sessions", "post", "exchangeDeviceAuthorization", "DeviceSessionRequest", "200", "Session"},
	{"/v1/auth/device/sessions", "post", "exchangeDeviceAuthorization", "DeviceSessionRequest", "428", "Error"},
	{"/v1/auth/sessions/refresh", "post", "refreshSession", "RefreshSessionRequest", "200", "Session"},
	{"/v1/auth/sessions/revoke", "post", "revokeSessionWithProof", "RefreshSessionRequest", "204", ""},
	{"/v1/auth/session", "delete", "deleteCurrentSession", "", "204", ""},
	{"/v1/auth/me", "get", "getCurrentUser", "", "200", "CurrentUser"},
	{"/v1/auth/devices", "get", "listDevices", "", "200", "DeviceList"},
	{"/v1/auth/devices/{deviceId}", "delete", "revokeDevice", "", "204", ""},
}

var schemaFields = map[string][]string{
	"DeviceAuthorizationRequest": {"deviceName", "devicePublicKey", "platform"},
	"DeviceAuthorization":        {"challenge", "deviceCode", "expiresIn", "interval", "userCode", "verificationUri", "verificationUriComplete"},
	"DeviceSessionRequest":       {"deviceCode", "proof"},
	"RefreshSessionRequest":      {"deviceId", "proof", "refreshToken"},
	"Session":                    {"accessToken", "deviceId", "expiresIn", "refreshToken"},
	"User":                       {"displayName", "email", "id", "status"},
	"CurrentUser":                {"device", "user"},
	"Device":                     {"createdAt", "id", "lastUsedAt", "name", "platform", "status"},
	"DeviceList":                 {"items"},
	"Error":                      {"code", "message", "requestId"},
}

func main() {
	check := flag.Bool("check", false, "fail if the checked-in client differs")
	flag.Parse()
	root, err := repositoryRoot()
	fatalIf(err)
	contractPath := filepath.Join(root, "packages", "contracts", "openapi.json")
	outputPath := filepath.Join(root, "internal", "client", "client.gen.go")
	contract, err := os.ReadFile(contractPath)
	fatalIf(err)
	digest := sha256.Sum256(contract)
	digestString := hex.EncodeToString(digest[:])
	if digestString != supportedContractSHA256 {
		fatalIf(fmt.Errorf("OpenAPI contract %s is not represented by the pinned client template %s", digestString, supportedContractSHA256))
	}
	var document map[string]any
	fatalIf(json.Unmarshal(contract, &document))
	fatalIf(validate(document, string(clientTemplate)))
	generated := render(digestString)
	if *check {
		current, err := os.ReadFile(outputPath)
		fatalIf(err)
		if !bytes.Equal(current, generated) {
			fatalIf(fmt.Errorf("generated client is stale; run 'make generate-client'"))
		}
		return
	}
	temp, err := os.CreateTemp(filepath.Dir(outputPath), ".client.gen.go.*")
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

func validate(document map[string]any, template string) error {
	if value, _ := at(document, "openapi").(string); value != "3.1.0" {
		return fmt.Errorf("OpenAPI version must be 3.1.0")
	}
	if value, _ := at(document, "components", "securitySchemes", "bearerAuth", "bearerFormat").(string); value != "opaque" {
		return fmt.Errorf("bearerAuth must declare opaque tokens")
	}
	for _, expected := range operations {
		base := []string{"paths", expected.path, expected.method}
		if value, _ := at(document, append(base, "operationId")...).(string); value != expected.id {
			return fmt.Errorf("%s %s operationId is %q, want %q", expected.method, expected.path, value, expected.id)
		}
		if expected.requestRef != "" {
			value, _ := at(document, append(base, "requestBody", "content", "application/json", "schema", "$ref")...).(string)
			if value != schemaRef(expected.requestRef) {
				return fmt.Errorf("%s request schema is %q, want %q", expected.id, value, schemaRef(expected.requestRef))
			}
		}
		if expected.responseRef != "" {
			value, _ := at(document, append(base, "responses", expected.status, "content", "application/json", "schema", "$ref")...).(string)
			if value != schemaRef(expected.responseRef) {
				return fmt.Errorf("%s response %s schema is %q, want %q", expected.id, expected.status, value, schemaRef(expected.responseRef))
			}
		} else if at(document, append(base, "responses", expected.status)...) == nil {
			return fmt.Errorf("%s response %s is missing", expected.id, expected.status)
		}
	}
	for schema, expected := range schemaFields {
		object, ok := at(document, "components", "schemas", schema).(map[string]any)
		if !ok || object["type"] != "object" || object["additionalProperties"] != false {
			return fmt.Errorf("schema %s must be a closed object", schema)
		}
		properties, ok := object["properties"].(map[string]any)
		if !ok {
			return fmt.Errorf("schema %s has no properties", schema)
		}
		actual := make([]string, 0, len(properties))
		for field := range properties {
			actual = append(actual, field)
			if !strings.Contains(template, `json:"`+field) {
				return fmt.Errorf("generated template has no JSON field %s.%s", schema, field)
			}
		}
		sort.Strings(actual)
		if strings.Join(actual, ",") != strings.Join(expected, ",") {
			return fmt.Errorf("schema %s fields are %v, want %v", schema, actual, expected)
		}
	}
	return nil
}

func at(root any, path ...string) any {
	current := root
	for _, element := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[element]
	}
	return current
}

func schemaRef(name string) string { return "#/components/schemas/" + name }

func render(digest string) []byte {
	_, rest, found := bytes.Cut(clientTemplate, []byte("\n"))
	if !found {
		rest = clientTemplate
	}
	header := "// Code generated from packages/contracts/openapi.json; DO NOT EDIT.\n// Contract SHA256: " + digest + "\n"
	return append([]byte(header), rest...)
}

func fatalIf(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate-client:", err)
		os.Exit(1)
	}
}

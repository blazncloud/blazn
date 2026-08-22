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
	openAPISHA256          = "566c781b50ece53494e5c153085f5bb472aaabae8d729069de76782a3983217e"
	commonOpenAPISHA256    = "cbb5b7fa0d8add9a8f38ed36a0853704cfeb480d7a6051f3b8965c739e160e34"
	planSHA256             = "111984c682128e09a2caba46d405feb848c34e65ded478dbc49d9e74a677341e"
	receiptSHA256          = "cdfd07ec5c7fde1aa4501e006cdf8ddb060e7af33ab329af89de247d1c29a1e4"
	operationReceiptSHA256 = "95445951f5fb917e80668e45e0a82ebbed24735b575a16e8fdad56824214c79b"
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
	{"/v1/node-service/join-credentials/{issuanceId}/consume", "post", "consumeNodeJoinCredential", "200", "ConsumeJoinCredentialRequest", "Node", "nodeProof"},
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
	generated = bytes.ReplaceAll(generated, []byte("OPERATION_RECEIPT_SHA256"), []byte(operationReceiptSHA256))
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
		{path: filepath.Join("packages", "contracts", "openapi.json"), digest: commonOpenAPISHA256},
		{path: filepath.Join("packages", "contracts", "nodes", "node-install-plan.schema.json"), digest: planSHA256},
		{path: filepath.Join("packages", "contracts", "nodes", "node-install-receipt.schema.json"), digest: receiptSHA256},
		{path: filepath.Join("packages", "contracts", "nodes", "node-operation-receipt.schema.json"), digest: operationReceiptSHA256},
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
	if err := validateSharedNodeErrors(openAPI, sources[filepath.Join("packages", "contracts", "openapi.json")].doc); err != nil {
		return err
	}
	plan := sources[filepath.Join("packages", "contracts", "nodes", "node-install-plan.schema.json")].doc
	receipt := sources[filepath.Join("packages", "contracts", "nodes", "node-install-receipt.schema.json")].doc
	if err := validateInstallSchema(plan, "Blazn NodeInstallPlan", []string{"schemaVersion", "planId", "nodeId", "enrollmentId", "workspaceId", "idempotencyKey", "approvedBy", "approvedAt", "hostname", "mode", "installProfile", "cluster", "target", "registryTrust", "components", "nodeService", "labels", "taints", "resourceBounds", "mutations", "validationTests", "rollback", "issuedAt", "expiresAt", "signingKeyId", "digest", "signature"}); err != nil {
		return fmt.Errorf("install plan: %w", err)
	}
	if err := validateInstallSchema(receipt, "Blazn NodeInstallReceipt", []string{"schemaVersion", "receiptId", "planId", "planDigest", "nodeId", "generation", "nodeIdentityGeneration", "signerKind", "signerFingerprint", "state", "currentStage", "owner", "binary", "service", "mutations", "residues", "createdAt", "updatedAt", "signingKeyId", "digest", "signature"}); err != nil {
		return fmt.Errorf("install receipt: %w", err)
	}
	operationReceipt := sources[filepath.Join("packages", "contracts", "nodes", "node-operation-receipt.schema.json")].doc
	if err := validateOperationReceiptSchema(operationReceipt); err != nil {
		return fmt.Errorf("operation receipt: %w", err)
	}
	for _, marker := range []string{
		"func ValidateNodeInstallPlan(", "func ValidateNodeInstallReceipt(", "func ValidateNodeOperationReceipt(",
		"func VerifyNodeInstallPlan(", "func VerifyNodeInstallReceipt(", "func VerifyNodeOperationReceipt(", "func NodeCapabilityDigest(",
		"func DeriveNodeEnrollmentToken(", "func SealNodeJoinCredential(", "func OpenNodeJoinCredential(",
		"type NodeError = ErrorBody",
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
	securitySchemes, ok := at(document, "components", "securitySchemes").(map[string]any)
	bearerScheme, bearerOK := securitySchemes["bearerAuth"].(map[string]any)
	nodeProofScheme, nodeProofOK := securitySchemes["nodeProof"].(map[string]any)
	if !ok || len(securitySchemes) != 2 || !bearerOK || len(bearerScheme) != 3 || !nodeProofOK || len(nodeProofScheme) != 3 || atString(document, "components", "securitySchemes", "bearerAuth", "type") != "http" || atString(document, "components", "securitySchemes", "bearerAuth", "scheme") != "bearer" || atString(document, "components", "securitySchemes", "bearerAuth", "bearerFormat") != "opaque" || atString(document, "components", "securitySchemes", "nodeProof", "type") != "apiKey" || atString(document, "components", "securitySchemes", "nodeProof", "in") != "header" || atString(document, "components", "securitySchemes", "nodeProof", "name") != "X-Blazn-Node-Proof" {
		return fmt.Errorf("node authentication schemes changed")
	}
	globalSecurity, ok := at(document, "security").([]any)
	if !ok || len(globalSecurity) != 1 || !securityRequirementIs(globalSecurity[0], "bearerAuth") {
		return fmt.Errorf("node global bearer authentication changed")
	}
	if err := validateNodeError(document); err != nil {
		return err
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
			values, ok := security.([]any)
			if !ok || len(values) != 1 || !securityRequirementIs(values[0], "nodeProof") {
				return fmt.Errorf("%s must require only nodeProof", expected.id)
			}
		case "bearer":
			if security != nil {
				return fmt.Errorf("%s must inherit global bearer auth", expected.id)
			}
		}
		needsIdempotency := expected.id == "createNodeEnrollment" || expected.id == "createNodeOperation" || expected.id == "issueNodeJoinCredential" || expected.id == "consumeNodeJoinCredential"
		parameters, _ := at(document, append(base, "parameters")...).([]any)
		hasIdempotency := false
		for _, parameter := range parameters {
			if atString(parameter, "$ref") == "#/components/parameters/IdempotencyKey" {
				hasIdempotency = true
			}
		}
		if needsIdempotency != hasIdempotency {
			return fmt.Errorf("%s idempotency header contract changed", expected.id)
		}
	}
	if atString(document, "components", "schemas", "ExchangeNodeEnrollmentRequest", "properties", "token", "type") != "string" {
		return fmt.Errorf("enrollment token must remain in JSON request body")
	}
	capability, ok := at(document, "components", "schemas", "NodeCapability").(map[string]any)
	if !ok || capability["additionalProperties"] != false {
		return fmt.Errorf("NodeCapability must remain a closed object")
	}
	for _, field := range []string{"version", "host", "worker", "sandboxBackends", "runtimeClasses", "localModels"} {
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

func securityRequirementIs(value any, scheme string) bool {
	requirement, ok := value.(map[string]any)
	if !ok || len(requirement) != 1 {
		return false
	}
	scopes, ok := requirement[scheme].([]any)
	return ok && len(scopes) == 0
}

func validateNodeError(document map[string]any) error {
	if atString(document, "components", "responses", "Error", "content", "application/json", "schema", "$ref") != "#/components/schemas/NodeError" {
		return fmt.Errorf("node error response schema changed")
	}
	errorSchema, ok := at(document, "components", "schemas", "NodeError").(map[string]any)
	if !ok || errorSchema["type"] != "object" || errorSchema["additionalProperties"] != false {
		return fmt.Errorf("NodeError must remain a closed object")
	}
	properties, ok := errorSchema["properties"].(map[string]any)
	if !ok || len(properties) != 3 || properties["code"] == nil || properties["message"] == nil || properties["requestId"] == nil {
		return fmt.Errorf("NodeError fields changed")
	}
	requiredValues, ok := errorSchema["required"].([]any)
	required := make([]string, 0, len(requiredValues))
	if !ok {
		return fmt.Errorf("NodeError required fields changed")
	}
	for _, value := range requiredValues {
		field, fieldOK := value.(string)
		if !fieldOK {
			return fmt.Errorf("NodeError required fields changed")
		}
		required = append(required, field)
	}
	sort.Strings(required)
	if strings.Join(required, ",") != "code,message,requestId" {
		return fmt.Errorf("NodeError required fields changed")
	}
	if atString(errorSchema, "properties", "code", "type") != "string" || atString(errorSchema, "properties", "message", "type") != "string" || atNumber(errorSchema, "properties", "message", "minLength") != 1 || atNumber(errorSchema, "properties", "message", "maxLength") != 1024 || atString(errorSchema, "properties", "requestId", "type") != "string" || atNumber(errorSchema, "properties", "requestId", "minLength") != 1 || atNumber(errorSchema, "properties", "requestId", "maxLength") != 128 {
		return fmt.Errorf("NodeError field bounds changed")
	}
	want := map[string]int{
		"access_expired": 401, "authorization_capacity": 503, "authorization_not_found": 404, "authorization_pending": 428,
		"capability_digest_invalid": 400, "device_not_found": 404, "device_proof_invalid": 403, "device_revoked": 401,
		"enrollment_consumed": 410, "enrollment_expired": 410, "enrollment_invalid": 400, "enrollment_not_found": 404,
		"expired_token": 400, "forwarded_identity_invalid": 400, "heartbeat_replay": 409, "heartbeat_skew": 400,
		"identity_rejected": 403, "idempotency_conflict": 409, "internal_error": 500, "invalid_json": 400,
		"invalid_public_key": 400, "invalid_request": 400, "join_credential_consumed": 410, "join_credential_invalid": 400,
		"membership_required": 403, "method_not_allowed": 405, "node_not_found": 404, "not_found": 404,
		"object_storage_unavailable": 503, "permission_denied": 403, "proxy_auth_invalid": 403, "rate_limited": 429,
		"request_too_large": 413, "session_revoked": 401, "slow_down": 429, "state_conflict": 409,
		"unauthorized": 401, "version_conflict": 409,
	}
	statuses, ok := errorSchema["x-blazn-error-status"].(map[string]any)
	if !ok || len(statuses) != len(want) {
		return fmt.Errorf("NodeError status map changed")
	}
	values, ok := at(document, "components", "schemas", "NodeError", "properties", "code", "enum").([]any)
	if !ok || len(values) != len(want) {
		return fmt.Errorf("NodeError code enum changed")
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		code, ok := value.(string)
		status, statusOK := statuses[code].(float64)
		if !ok || seen[code] || want[code] == 0 || !statusOK || status != float64(want[code]) {
			return fmt.Errorf("NodeError code/status changed: %v", value)
		}
		seen[code] = true
	}
	if len(seen) != len(want) {
		return fmt.Errorf("NodeError code set is incomplete")
	}
	return nil
}

func validateSharedNodeErrors(node, common map[string]any) error {
	nodeStatuses, ok := at(node, "components", "schemas", "NodeError", "x-blazn-error-status").(map[string]any)
	if !ok {
		return fmt.Errorf("NodeError status map is unavailable")
	}
	commonStatuses, ok := at(common, "components", "schemas", "Error", "x-blazn-error-status").(map[string]any)
	if !ok {
		return fmt.Errorf("common Error status map is unavailable")
	}
	for code, commonStatus := range commonStatuses {
		if nodeStatus, exists := nodeStatuses[code]; exists && nodeStatus != commonStatus {
			return fmt.Errorf("shared error status differs for %s", code)
		}
	}
	variants, ok := at(node, "components", "schemas", "NodeOperation", "properties", "error", "oneOf").([]any)
	if !ok || len(variants) != 2 || atString(variants[0], "$ref") != "#/components/schemas/NodeError" {
		return fmt.Errorf("NodeOperation error must reference local NodeError")
	}
	return validateLocalRefs(node)
}

func validateLocalRefs(document map[string]any) error {
	var walk func(any) error
	walk = func(value any) error {
		switch typed := value.(type) {
		case map[string]any:
			if ref, ok := typed["$ref"].(string); ok && strings.HasPrefix(ref, "#/") {
				parts := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
				if at(document, parts...) == nil {
					return fmt.Errorf("unresolved local reference %s", ref)
				}
			}
			for _, child := range typed {
				if err := walk(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range typed {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(document)
}

func validateOperationReceiptSchema(document map[string]any) error {
	if atString(document, "$schema") != "https://json-schema.org/draft/2020-12/schema" || atString(document, "title") != "Blazn NodeOperationReceipt" || atString(document, "type") != "object" || at(document, "additionalProperties") != false {
		return fmt.Errorf("must remain a closed JSON Schema 2020-12 operation receipt")
	}
	if atString(document, "properties", "schemaVersion", "const") != "nodes/v1alpha1" || atNumber(document, "properties", "actions", "maxItems") != 4096 || atNumber(document, "properties", "residues", "maxItems") != 4096 {
		return fmt.Errorf("operation receipt bounds changed")
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

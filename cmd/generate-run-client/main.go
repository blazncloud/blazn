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

//go:embed run.gen.go.tmpl
var runTemplate []byte

const supportedRunContractSHA256 = "6a1b91ddc98d937e812f32d79234a4cd367c669ec1ad778e69f8c8f6d1d10919"

var operations = map[string]string{
	"POST /v1/workspaces/{workspaceId}/projects/{projectId}/runs":                                      "createRun",
	"GET /v1/workspaces/{workspaceId}/projects/{projectId}/runs":                                       "listRuns",
	"GET /v1/workspaces/{workspaceId}/projects/{projectId}/runs/{runId}":                               "getRun",
	"POST /v1/workspaces/{workspaceId}/projects/{projectId}/runs/{runId}/cancel":                       "cancelRun",
	"GET /v1/workspaces/{workspaceId}/projects/{projectId}/runs/{runId}/messages":                      "listRunMessages",
	"POST /v1/workspaces/{workspaceId}/projects/{projectId}/runs/{runId}/messages":                     "sendRunMessage",
	"POST /v1/workspaces/{workspaceId}/projects/{projectId}/runs/{runId}/messages/claim":               "claimRunMessage",
	"POST /v1/workspaces/{workspaceId}/projects/{projectId}/runs/{runId}/messages/{messageId}/deliver": "deliverRunMessage",
	"POST /v1/workspaces/{workspaceId}/projects/{projectId}/runs/{runId}/synthetic/progress":           "recordSyntheticRunProgress",
	"POST /v1/workspaces/{workspaceId}/projects/{projectId}/runs/{runId}/synthetic/complete":           "completeSyntheticRun",
	"POST /v1/workspaces/{workspaceId}/projects/{projectId}/runs/{runId}/artifacts":                    "uploadSyntheticRunArtifact",
	"GET /v1/workspaces/{workspaceId}/projects/{projectId}/runs/{runId}/artifacts":                     "listRunArtifacts",
	"GET /v1/workspaces/{workspaceId}/projects/{projectId}/runs/{runId}/events":                        "listRunEvents",
	"GET /v1/workspaces/{workspaceId}/projects/{projectId}/runs/{runId}/progress":                      "listRunProgress",
	"GET /v1/workspaces/{workspaceId}/projects/{projectId}/artifacts":                                  "listArtifacts",
	"GET /v1/workspaces/{workspaceId}/projects/{projectId}/artifacts/{artifactId}":                     "getArtifact",
}

var schemaFields = map[string][]string{
	"Run":          {"completedAt", "createdAt", "errorCode", "id", "inputArtifactIds", "kind", "outputNames", "placement", "planDigest", "projectId", "proofClass", "receipt", "requestedBy", "startedAt", "status", "version", "workspaceId"},
	"RunPlacement": {"modelRouteId", "nodeId", "sandboxId"},
	"RunReceipt":   {"artifactIds", "outcome", "planDigest", "proofClass", "schemaVersion", "summary"},
	"RunEnvelope":  {"run"}, "RunList": {"items", "nextCursor"},
	"CreateRunRequest": {"inputArtifactIds", "kind", "outputNames", "planDigest", "proofClass"}, "CancelRunRequest": {"expectedVersion"},
	"RunMessage": {"content", "contentDigest", "createdAt", "createdBy", "id", "kind", "ordinal", "parentMessageId", "projectId", "role", "runId", "status", "workspaceId"}, "SendRunMessageRequest": {"content", "kind", "parentMessageId"},
	"RunMessageEnvelope": {"message"}, "RunMessageList": {"items", "nextCursor"},
	"ClaimRunMessageRequest": {"leaseSeconds"}, "DeliverRunMessageRequest": {"claimId"}, "RunMessageClaim": {"claimId", "leaseExpiresAt", "message"}, "RunMessageClaimEnvelope": {"claim"},
	"SyntheticRunProgressRequest": {"message", "percent", "phase", "sequence"}, "ProgressAck": {"runId", "runVersion", "sequence", "status"},
	"CompleteSyntheticRunRequest": {"artifactIds", "expectedVersion", "planDigest", "summary"}, "RunReceiptSummary": {"steps", "warnings"},
	"ArtifactUploadMetadata": {"digest", "kind", "mediaType", "name", "sizeBytes"},
	"Artifact":               {"createdAt", "createdBy", "digest", "downloadAvailable", "id", "kind", "mediaType", "name", "projectId", "sizeBytes", "sourceRunId", "status", "updatedAt", "version", "workspaceId"},
	"ArtifactEnvelope":       {"artifact"}, "ArtifactList": {"items", "nextCursor"}, "RunError": {"code", "message", "requestId"},
	"RunEvent": {"createdAt", "payload", "sequence", "type"}, "RunEventList": {"items", "nextCursor"},
	"RunProgressEntry": {"createdAt", "message", "percent", "phase", "sequence"}, "RunProgressList": {"items"},
}

var schemaRequired = map[string][]string{
	"Run":          {"createdAt", "id", "inputArtifactIds", "kind", "outputNames", "placement", "planDigest", "projectId", "proofClass", "receipt", "requestedBy", "status", "version", "workspaceId"},
	"RunPlacement": {},
	"RunReceipt":   {"artifactIds", "outcome", "planDigest", "proofClass", "schemaVersion", "summary"},
	"RunEnvelope":  {"run"}, "RunList": {"items", "nextCursor"},
	"CreateRunRequest": {"inputArtifactIds", "kind", "outputNames", "planDigest", "proofClass"}, "CancelRunRequest": {"expectedVersion"},
	"RunMessage": {"content", "contentDigest", "createdAt", "createdBy", "id", "kind", "ordinal", "projectId", "role", "runId", "status", "workspaceId"}, "SendRunMessageRequest": {"content", "kind"},
	"RunMessageEnvelope": {"message"}, "RunMessageList": {"items", "nextCursor"},
	"ClaimRunMessageRequest": {"leaseSeconds"}, "DeliverRunMessageRequest": {"claimId"}, "RunMessageClaim": {"claimId", "leaseExpiresAt", "message"}, "RunMessageClaimEnvelope": {"claim"},
	"SyntheticRunProgressRequest": {"percent", "phase", "sequence"}, "ProgressAck": {"runId", "runVersion", "sequence", "status"},
	"CompleteSyntheticRunRequest": {"artifactIds", "expectedVersion", "planDigest", "summary"}, "RunReceiptSummary": {"steps", "warnings"},
	"ArtifactUploadMetadata": {"digest", "kind", "mediaType", "name", "sizeBytes"},
	"Artifact":               {"createdAt", "createdBy", "downloadAvailable", "id", "kind", "mediaType", "name", "projectId", "status", "updatedAt", "version", "workspaceId"},
	"ArtifactEnvelope":       {"artifact"}, "ArtifactList": {"items", "nextCursor"}, "RunError": {"code", "message", "requestId"},
	"RunEvent": {"createdAt", "payload", "sequence", "type"}, "RunEventList": {"items", "nextCursor"},
	"RunProgressEntry": {"createdAt", "percent", "phase", "sequence"}, "RunProgressList": {"items"},
}

func main() {
	check := flag.Bool("check", false, "fail if the checked-in Run client differs")
	flag.Parse()
	root, err := repositoryRoot()
	fatalIf(err)
	contractPath := filepath.Join(root, "packages", "contracts", "runs.openapi.json")
	outputPath := filepath.Join(root, "internal", "client", "run.gen.go")
	contract, err := os.ReadFile(contractPath)
	fatalIf(err)
	digest := sha256.Sum256(contract)
	digestString := hex.EncodeToString(digest[:])
	if digestString != supportedRunContractSHA256 {
		fatalIf(fmt.Errorf("Run OpenAPI contract %s is not represented by pinned client %s", digestString, supportedRunContractSHA256))
	}
	var document map[string]any
	fatalIf(json.Unmarshal(contract, &document))
	fatalIf(validate(document, string(runTemplate)))
	generated, err := format.Source(bytes.ReplaceAll(runTemplate, []byte("{{CONTRACT_SHA256}}"), []byte(digestString)))
	fatalIf(err)
	if *check {
		current, err := os.ReadFile(outputPath)
		fatalIf(err)
		if !bytes.Equal(current, generated) {
			fatalIf(fmt.Errorf("generated Run client is stale; run 'make generate-run-client'"))
		}
		return
	}
	temp, err := os.CreateTemp(filepath.Dir(outputPath), ".run.gen.go.*")
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
		return fmt.Errorf("Run contract version changed")
	}
	if stringAt(document, "servers", "0", "url") != "https://blazn.benpelo.com" {
		return fmt.Errorf("Run server origin changed")
	}
	paths, ok := valueAt(document, "paths").(map[string]any)
	if !ok || len(paths) != 13 {
		return fmt.Errorf("Run paths changed")
	}
	seen := map[string]string{}
	for path, raw := range paths {
		methods, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("Run path %s is invalid", path)
		}
		for method, operation := range methods {
			seen[strings.ToUpper(method)+" "+path] = stringAt(operation, "operationId")
		}
	}
	if fmt.Sprint(seen) == "" || len(seen) != len(operations) {
		return fmt.Errorf("Run operations changed")
	}
	for key, operationID := range operations {
		if seen[key] != operationID {
			return fmt.Errorf("Run operation %s=%q want %q", key, seen[key], operationID)
		}
		method := strings.ToLower(strings.SplitN(key, " ", 2)[0])
		path := strings.SplitN(key, " ", 2)[1]
		if stringAt(paths, path, method, "responses", "default", "$ref") != "#/components/responses/RunError" {
			return fmt.Errorf("Run operation %s lost its default error", operationID)
		}
	}
	schemas, ok := valueAt(document, "components", "schemas").(map[string]any)
	if !ok {
		return fmt.Errorf("Run schemas are missing")
	}
	for name, fields := range schemaFields {
		schema, ok := schemas[name].(map[string]any)
		if !ok || schema["additionalProperties"] != false {
			return fmt.Errorf("Run schema %s must be closed", name)
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			return fmt.Errorf("Run schema %s properties are missing", name)
		}
		got := keys(properties)
		if strings.Join(got, ",") != strings.Join(fields, ",") {
			return fmt.Errorf("Run schema %s fields=%v want %v", name, got, fields)
		}
		required, _ := schema["required"].([]any)
		requiredNames := make([]string, 0, len(required))
		for _, value := range required {
			field, ok := value.(string)
			if !ok {
				return fmt.Errorf("Run schema %s has a non-string required field", name)
			}
			requiredNames = append(requiredNames, field)
		}
		sort.Strings(requiredNames)
		if strings.Join(requiredNames, ",") != strings.Join(schemaRequired[name], ",") {
			return fmt.Errorf("Run schema %s required=%v want %v", name, requiredNames, schemaRequired[name])
		}
	}
	for name, want := range map[string][]string{
		"ProofClass":        {"local", "provider", "sandbox", "synthetic"},
		"RunStatus":         {"cancelled", "failed", "queued", "running", "succeeded"},
		"ArtifactMediaType": {"audio", "data", "document", "image", "other", "video"},
		"RunMessageKind":    {"followup", "prompt", "steer"},
	} {
		got := stringSliceAt(schemas, name, "enum")
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			return fmt.Errorf("Run schema %s enum changed", name)
		}
	}
	artifact := schemas["Artifact"].(map[string]any)
	for field, want := range map[string][]string{
		"mediaType": {"audio", "data", "document", "image", "other", "video"},
		"status":    {"deleted", "failed", "pending", "ready"},
	} {
		got := stringSliceAt(artifact, "properties", field, "enum")
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			return fmt.Errorf("Artifact %s enum changed", field)
		}
	}
	for _, marker := range []string{"func (c *Client) CreateRun", "func (c *Client) ListRuns", "func (c *Client) GetRun", "func (c *Client) CancelRun", "func (c *Client) ListRunMessages", "func (c *Client) SendRunMessage", "func (c *Client) ClaimRunMessage", "func (c *Client) DeliverRunMessage", "func (c *Client) RecordSyntheticRunProgress", "func (c *Client) CompleteSyntheticRun", "func (c *Client) UploadSyntheticRunArtifact", "func (c *Client) ListArtifacts", "func (c *Client) GetArtifact", "type RunError = ErrorBody"} {
		if !strings.Contains(template, marker) {
			return fmt.Errorf("Run template lacks %s", marker)
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

func stringSliceAt(value any, path ...string) []string {
	values, _ := valueAt(value, path...).([]any)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

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

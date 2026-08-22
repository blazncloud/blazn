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

//go:embed contracts.gen.go.tmpl
var contractTemplate []byte

var pinnedDigests = map[string]string{
	"activation-journal.schema.json":      "2f58611a5be80c932374bedeafc765018e629c2d106259d8b4776a321f0ddb67",
	"activation-receipt.schema.json":      "fd41f579f30433ab2c0c57ac364bcf4fc603ef293ac3b39cf71e310e7c99e6ee",
	"event.schema.json":                   "e61f1a558c744122c36427d692bbb9b7c7b620e84d75612b2e60e9c17d0310d3",
	"normalized-error.schema.json":        "3f05faaa510ee0a97fc6e6b8a5bc5dea830c3a17edd64476777b8b847532bd1c",
	"normalized-request.schema.json":      "40999c0f9f2109515dd4784d79ba92a2474c22d9cc9a8bee8b60b57699104778",
	"normalized-response.schema.json":     "16b383d0a61bd87d3d0983dd813d2940badb095cac6660642c4e8f5114d02285",
	"normalized-stream-event.schema.json": "a8e30211bd54861dfbb04568802da8f6f476e4ad7eff21dbbb9ba11809a85e4b",
	"policy.schema.json":                  "36c42efd44aca0de01280fb190001aac2bf06dac83d870ba7a3f211b12c84bc9",
}

var requiredFields = map[string][]string{
	"activation-journal.schema.json":      {"activationId", "binary", "checksum", "createdAt", "environment", "generation", "listener", "mode", "nonce", "ownerUid", "platform", "policy", "rollbackActions", "schemaVersion", "sessionIdentity", "state", "updatedAt"},
	"activation-receipt.schema.json":      {"activatedAt", "activationId", "checksum", "environment", "generation", "journalDigest", "listener", "mode", "platform", "policyDigest", "publicationMechanism", "schemaVersion", "sessionIdentity", "state"},
	"event.schema.json":                   {"activationId", "attempt", "cursor", "destinationClass", "eventId", "latencyMs", "logicalRequestId", "modelAlias", "outcome", "policy", "protocol", "reasonCode", "routeId", "timestamp", "type"},
	"normalized-error.schema.json":        {"code", "retryClass", "safeMessage"},
	"normalized-request.schema.json":      {"blocks", "capabilitiesRequired", "dataClass", "limits", "logicalRequestId", "modelAlias", "protocol", "stream", "tools"},
	"normalized-response.schema.json":     {"blocks", "finishReason", "logicalRequestId", "modelAlias", "routeId", "usage"},
	"normalized-stream-event.schema.json": {"logicalRequestId", "sequence", "type"},
	"policy.schema.json":                  {"aliases", "contentCapture", "fallback", "id", "protocols", "requestLimits", "routes", "version", "workspaceId"},
}

var propertyFields = map[string][]string{
	"activation-journal.schema.json":      {"activationId", "binary", "ca", "checksum", "createdAt", "environment", "generation", "listener", "mode", "nonce", "ownerUid", "platform", "policy", "rollbackActions", "schemaVersion", "sessionIdentity", "state", "updatedAt"},
	"activation-receipt.schema.json":      {"activatedAt", "activationId", "checksum", "environment", "generation", "journalDigest", "listener", "mode", "platform", "policyDigest", "publicationMechanism", "schemaVersion", "sessionIdentity", "state"},
	"event.schema.json":                   {"activationId", "attempt", "cursor", "destinationClass", "eventId", "latencyMs", "logicalRequestId", "modelAlias", "outcome", "policy", "protocol", "reasonCode", "routeId", "timestamp", "type", "usage"},
	"normalized-error.schema.json":        {"code", "retryClass", "safeMessage", "upstreamStatus"},
	"normalized-request.schema.json":      {"blocks", "capabilitiesRequired", "dataClass", "limits", "logicalRequestId", "modelAlias", "protocol", "responseSchema", "stream", "toolChoice", "tools"},
	"normalized-response.schema.json":     {"blocks", "finishReason", "logicalRequestId", "modelAlias", "routeId", "usage"},
	"normalized-stream-event.schema.json": {"argumentsDelta", "callId", "error", "finishReason", "logicalRequestId", "sequence", "text", "toolName", "type", "usage"},
	"policy.schema.json":                  {"aliases", "contentCapture", "fallback", "id", "protocols", "requestLimits", "routes", "version", "workspaceId"},
}

func main() {
	check := flag.Bool("check", false, "fail if generated proxy contracts differ")
	flag.Parse()
	root, err := repositoryRoot()
	fatalIf(err)
	contractDir := filepath.Join(root, "packages", "contracts", "proxy")
	digests := make(map[string]string, len(pinnedDigests))
	documents := make(map[string]map[string]any, len(pinnedDigests))
	for name, pinned := range pinnedDigests {
		encoded, err := os.ReadFile(filepath.Join(contractDir, name))
		fatalIf(err)
		digest := sha256.Sum256(encoded)
		actual := hex.EncodeToString(digest[:])
		if actual != pinned {
			fatalIf(fmt.Errorf("%s digest=%s pinned=%s", name, actual, pinned))
		}
		var document map[string]any
		fatalIf(json.Unmarshal(encoded, &document))
		digests[name] = actual
		documents[name] = document
	}
	fatalIf(validate(documents, string(contractTemplate)))
	generated, err := format.Source(render(digests))
	fatalIf(err)
	output := filepath.Join(root, "internal", "proxycontract", "contracts.gen.go")
	if *check {
		current, err := os.ReadFile(output)
		fatalIf(err)
		if !bytes.Equal(current, generated) {
			fatalIf(errorsNew("generated proxy contracts are stale; run 'make generate-proxy-contract'"))
		}
		return
	}
	fatalIf(os.MkdirAll(filepath.Dir(output), 0o755))
	temp, err := os.CreateTemp(filepath.Dir(output), ".contracts.gen.go.*")
	fatalIf(err)
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	_, err = temp.Write(generated)
	fatalIf(err)
	fatalIf(temp.Sync())
	fatalIf(temp.Close())
	fatalIf(os.Rename(tempPath, output))
}

type generatedError string

func (e generatedError) Error() string { return string(e) }
func errorsNew(value string) error     { return generatedError(value) }

func validate(documents map[string]map[string]any, template string) error {
	for name, document := range documents {
		if atString(document, "$schema") != "https://json-schema.org/draft/2020-12/schema" || atString(document, "type") != "object" || at(document, "additionalProperties") != false {
			return fmt.Errorf("%s must be a closed draft-2020-12 object", name)
		}
		if got := sortedObjectKeys(at(document, "properties")); strings.Join(got, ",") != strings.Join(propertyFields[name], ",") {
			return fmt.Errorf("%s properties=%v want=%v", name, got, propertyFields[name])
		}
		if got := sortedStrings(at(document, "required")); strings.Join(got, ",") != strings.Join(requiredFields[name], ",") {
			return fmt.Errorf("%s required=%v want=%v", name, got, requiredFields[name])
		}
	}
	if err := validatePolicy(documents["policy.schema.json"]); err != nil {
		return err
	}
	if err := validateJournal(documents["activation-journal.schema.json"]); err != nil {
		return err
	}
	if err := validateReceipt(documents["activation-receipt.schema.json"]); err != nil {
		return err
	}
	if err := validateEvent(documents["event.schema.json"]); err != nil {
		return err
	}
	if err := validateNormalized(documents); err != nil {
		return err
	}
	for _, tag := range []string{"workspaceId", "contentCapture", "rollbackActions", "listenerKeyFingerprint", "publicationMechanism", "logicalRequestId", "destinationClass", "inputTokens", "outputTokens", "safeMessage", "argumentsDelta"} {
		if !strings.Contains(template, `json:"`+tag) {
			return fmt.Errorf("generated template lacks JSON field %s", tag)
		}
	}
	return nil
}

func validatePolicy(document map[string]any) error {
	if at(document, "properties", "contentCapture", "const") != false {
		return errorsNew("policy contentCapture must be const false")
	}
	if atNumber(document, "properties", "fallback", "properties", "maxAttempts", "minimum") != 1 || atNumber(document, "properties", "fallback", "properties", "maxAttempts", "maximum") != 2 {
		return errorsNew("policy fallback maxAttempts must be 1..2")
	}
	aliasBase := []string{"properties", "aliases", "patternProperties", "^[a-zA-Z0-9._:-]{1,128}$"}
	if at(document, append(aliasBase, "additionalProperties")...) != false || atNumber(document, append(aliasBase, "properties", "routeIds", "maxItems")...) != 2 || atString(document, append(aliasBase, "properties", "routeIds", "items", "format")...) != "uuid" {
		return errorsNew("policy alias contract changed")
	}
	if got := sortedStrings(at(document, "properties", "protocols", "items", "enum")); strings.Join(got, ",") != "anthropic-messages,openai-chat,openai-responses" {
		return fmt.Errorf("policy protocols changed: %v", got)
	}
	if got := sortedStrings(at(document, "properties", "routes", "items", "required")); strings.Join(got, ",") != "acceptedDataClasses,capabilities,costClass,credentialRef,dataBoundary,destinationClass,destinationProtocol,endpoint,healthTimeoutMs,id,model,sourceProtocol" {
		return fmt.Errorf("policy route fields changed: %v", got)
	}
	if atNumber(document, "properties", "routes", "items", "properties", "endpoint", "properties", "port", "maximum") != 65535 || lenArray(at(document, "properties", "routes", "items", "allOf")) != 2 {
		return errorsNew("policy endpoint/class constraints changed")
	}
	if atNumber(document, "properties", "fallback", "properties", "maxAttempts", "maximum") != 2 || atNumber(document, "properties", "fallback", "properties", "allowedBoundaryTransitions", "maxItems") != 3 {
		return errorsNew("policy fallback bounds changed")
	}
	return nil
}

func validateJournal(document map[string]any) error {
	if atString(document, "properties", "checksum", "pattern") != "^sha256:[0-9a-f]{64}$" || atString(document, "properties", "policy", "properties", "digest", "pattern") != "^sha256:[0-9a-f]{64}$" {
		return errorsNew("journal checksum/digest patterns changed")
	}
	if got := sortedStrings(at(document, "properties", "state", "enum")); strings.Join(got, ",") != "active,deactivating,prepared,publishing,recovery_required" {
		return fmt.Errorf("journal states changed: %v", got)
	}
	if got := sortedStrings(at(document, "properties", "environment", "items", "properties", "name", "enum")); strings.Join(got, ",") != "ANTHROPIC_API_KEY,ANTHROPIC_AUTH_TOKEN,ANTHROPIC_BASE_URL,OPENAI_API_KEY,OPENAI_BASE_URL" {
		return fmt.Errorf("journal environment names changed: %v", got)
	}
	if atNumber(document, "properties", "environment", "maxItems") != 16 || atNumber(document, "properties", "listener", "properties", "pid", "minimum") != 1 {
		return errorsNew("journal environment/listener bounds changed")
	}
	if got := sortedStrings(at(document, "properties", "environment", "items", "properties", "rollbackAction", "enum")); strings.Join(got, ",") != "remove_blazn_value,restore_prior_value" {
		return fmt.Errorf("journal rollback actions changed: %v", got)
	}
	if lenArray(at(document, "properties", "environment", "items", "allOf")) != 2 || atString(document, "properties", "ca", "type") != "null" {
		return errorsNew("journal conditional/CA constraints changed")
	}
	if got := sortedStrings(at(document, "properties", "rollbackActions", "items", "required")); strings.Join(got, ",") != "operation,ordinal,target" {
		return fmt.Errorf("journal rollback step fields changed: %v", got)
	}
	return nil
}

func validateReceipt(document map[string]any) error {
	if atString(document, "properties", "checksum", "pattern") != "^sha256:[0-9a-f]{64}$" || atString(document, "properties", "journalDigest", "pattern") != "^sha256:[0-9a-f]{64}$" {
		return errorsNew("receipt checksum patterns changed")
	}
	if got := sortedStrings(at(document, "properties", "environment", "items", "properties", "name", "enum")); strings.Join(got, ",") != "ANTHROPIC_API_KEY,ANTHROPIC_AUTH_TOKEN,ANTHROPIC_BASE_URL,OPENAI_API_KEY,OPENAI_BASE_URL" {
		return fmt.Errorf("receipt environment names changed: %v", got)
	}
	if got := sortedStrings(at(document, "properties", "mode", "enum")); strings.Join(got, ",") != "scoped_run,session" {
		return fmt.Errorf("receipt modes changed: %v", got)
	}
	return nil
}

func validateEvent(document map[string]any) error {
	if atNumber(document, "properties", "attempt", "minimum") != 1 || atNumber(document, "properties", "attempt", "maximum") != 2 || at(document, "properties", "usage", "additionalProperties") != false {
		return errorsNew("event attempt/usage bounds changed")
	}
	for _, forbidden := range []string{"authorization", "body", "content", "cookie", "credential", "headers", "messages", "prompt", "response", "tools"} {
		if at(document, "properties", forbidden) != nil {
			return fmt.Errorf("event schema contains forbidden content field %s", forbidden)
		}
	}
	if got := sortedStrings(at(document, "properties", "protocol", "enum")); strings.Join(got, ",") != "anthropic-messages,openai-chat,openai-responses" {
		return fmt.Errorf("event protocols changed: %v", got)
	}
	return nil
}

func validateNormalized(documents map[string]map[string]any) error {
	request := documents["normalized-request.schema.json"]
	if at(request, "additionalProperties") != false || atNumber(request, "properties", "limits", "properties", "stop", "maxItems") != 8 {
		return errorsNew("normalized request bounds changed")
	}
	if got := sortedStrings(at(request, "properties", "dataClass", "enum")); strings.Join(got, ",") != "company,local_only,public,restricted" {
		return fmt.Errorf("normalized data classes changed: %v", got)
	}
	stream := documents["normalized-stream-event.schema.json"]
	if atString(stream, "properties", "error", "$ref") != "normalized-error.schema.json" {
		return errorsNew("normalized stream error ref changed")
	}
	if at(documents["normalized-response.schema.json"], "additionalProperties") != false || at(documents["normalized-error.schema.json"], "additionalProperties") != false {
		return errorsNew("normalized schemas must remain closed")
	}
	return nil
}

func render(digests map[string]string) []byte {
	_, rest, found := bytes.Cut(contractTemplate, []byte("\n"))
	if !found {
		rest = contractTemplate
	}
	header := "// Code generated from packages/contracts/proxy; DO NOT EDIT.\n"
	names := make([]string, 0, len(digests))
	for name := range digests {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		header += "// " + name + " SHA256: " + digests[name] + "\n"
	}
	return append([]byte(header), rest...)
}

func sortedObjectKeys(value any) []string {
	object, _ := value.(map[string]any)
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
func sortedStrings(value any) []string {
	values, _ := value.([]any)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			result = append(result, text)
		}
	}
	sort.Strings(result)
	return result
}
func lenArray(value any) int                   { values, _ := value.([]any); return len(values) }
func atString(root any, path ...string) string { value, _ := at(root, path...).(string); return value }
func atNumber(root any, path ...string) float64 {
	value, _ := at(root, path...).(float64)
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

func repositoryRoot() (string, error) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return "", errorsNew("locate generator source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return "", err
	}
	return root, nil
}
func fatalIf(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate-proxy-contract:", err)
		os.Exit(1)
	}
}

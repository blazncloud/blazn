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

const pinnedPOCPolicyDigest = "4fe4ef2e0ea038d9c989731ca83f6846fa0e5a64949f9df42d0935903076a275"

var pinnedDigests = map[string]string{
	"activation-journal.schema.json":      "b5b75e0ae6ef54f645dfb6a1ec6743ff580cf6d91348566a30ebf3f63a710807",
	"activation-receipt.schema.json":      "93720291b499bc1af00a64957155666b6d88575733e9deb848adcdc76dcb7c5a",
	"event.schema.json":                   "d672f8ec2dd0eaab6200cb9a17b57f92fcef6e343f4ce9d7deb8aa4f09a2a704",
	"normalized-error.schema.json":        "3f05faaa510ee0a97fc6e6b8a5bc5dea830c3a17edd64476777b8b847532bd1c",
	"normalized-request.schema.json":      "2f1f41709c21e871c8d6e61333b493cf04d08070c59a48e1f908b262204237b4",
	"normalized-response.schema.json":     "90bc26e2bdf4cadcf061f69fe89dd14b8dca1da365d7c2bcde4f8289a2467f87",
	"normalized-stream-event.schema.json": "2be0990699fe1ae7de35627c997c2b23f6cb525d19c119b54ad65afec8707fea",
	"policy.schema.json":                  "3f8925f6fbdf3fab9613a88c5ce7e97e2c7d0cac702e0180f67ea28c92d5f7b9",
}

var requiredFields = map[string][]string{
	"activation-journal.schema.json":      {"activationId", "binary", "ca", "checksum", "createdAt", "environment", "generation", "listener", "mode", "nonce", "ownerUid", "platform", "policy", "rollbackActions", "schemaVersion", "sessionIdentity", "state", "updatedAt"},
	"activation-receipt.schema.json":      {"activatedAt", "activationId", "binary", "checksum", "environment", "generation", "journalDigest", "listener", "mode", "nonce", "ownerUid", "platform", "policyDigest", "publicationMechanism", "rollbackSummary", "schemaVersion", "sessionIdentity", "state"},
	"event.schema.json":                   {"activationId", "attempt", "cursor", "destinationClass", "eventId", "latencyMs", "logicalRequestId", "modelAlias", "outcome", "policy", "protocol", "reasonCode", "routeId", "timestamp", "type"},
	"normalized-error.schema.json":        {"code", "retryClass", "safeMessage"},
	"normalized-request.schema.json":      {"blocks", "capabilitiesRequired", "dataClass", "limits", "logicalRequestId", "modelAlias", "protocol", "stream", "tools"},
	"normalized-response.schema.json":     {"blocks", "finishReason", "logicalRequestId", "modelAlias", "routeId", "usage"},
	"normalized-stream-event.schema.json": {"logicalRequestId", "sequence", "type"},
	"policy.schema.json":                  {"aliases", "contentCapture", "fallback", "id", "protocols", "requestLimits", "routes", "version", "workspaceId"},
}

var propertyFields = map[string][]string{
	"activation-journal.schema.json":      {"activationId", "binary", "ca", "checksum", "createdAt", "environment", "generation", "listener", "mode", "nonce", "ownerUid", "platform", "policy", "rollbackActions", "schemaVersion", "sessionIdentity", "state", "updatedAt"},
	"activation-receipt.schema.json":      {"activatedAt", "activationId", "binary", "checksum", "environment", "generation", "journalDigest", "listener", "mode", "nonce", "ownerUid", "platform", "policyDigest", "publicationMechanism", "rollbackSummary", "schemaVersion", "sessionIdentity", "state"},
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
	fixture, err := os.ReadFile(filepath.Join(contractDir, "fixtures", "poc-policy.json"))
	fatalIf(err)
	fixtureSum := sha256.Sum256(fixture)
	if actual := hex.EncodeToString(fixtureSum[:]); actual != pinnedPOCPolicyDigest {
		fatalIf(fmt.Errorf("fixtures/poc-policy.json digest=%s pinned=%s", actual, pinnedPOCPolicyDigest))
	}
	fatalIf(validatePOCPolicyFixture(fixture))
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
	for _, tag := range []string{"workspaceId", "contentCapture", "rollbackActions", "rollbackSummary", "listenerKeyFingerprint", "publicationMechanism", "logicalRequestId", "destinationClass", "inputTokens", "outputTokens", "safeMessage", "argumentsDelta", "activationMarker", "ownerUid", "nonce"} {
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
	if got := sortedStrings(at(document, "properties", "routes", "items", "properties", "destinationProtocol", "enum")); strings.Join(got, ",") != "openai-chat,openai-responses" {
		return fmt.Errorf("POC destination protocols changed: %v", got)
	}
	if atString(document, "properties", "routes", "items", "properties", "credentialRef", "pattern") != "^(?:node-route|workspace-vault)://[A-Za-z0-9](?:[A-Za-z0-9._~-]*[A-Za-z0-9])?(?:/[A-Za-z0-9](?:[A-Za-z0-9._~-]*[A-Za-z0-9])?)*$" {
		return errorsNew("policy credential reference contract changed")
	}
	if got := sortedStrings(at(document, "properties", "routes", "items", "required")); strings.Join(got, ",") != "acceptedDataClasses,capabilities,costClass,credentialRef,dataBoundary,destinationClass,destinationProtocol,endpoint,healthTimeoutMs,id,model,sourceProtocols" {
		return fmt.Errorf("policy route fields changed: %v", got)
	}
	if atNumber(document, "properties", "routes", "items", "properties", "endpoint", "properties", "port", "maximum") != 65535 || lenArray(at(document, "properties", "routes", "items", "allOf")) != 4 {
		return errorsNew("policy endpoint/class constraints changed")
	}
	if atNumber(document, "properties", "fallback", "properties", "maxAttempts", "maximum") != 2 || atNumber(document, "properties", "fallback", "properties", "allowedBoundaryTransitions", "maxItems") != 3 {
		return errorsNew("policy fallback bounds changed")
	}
	return nil
}

func validatePOCPolicyFixture(encoded []byte) error {
	var policy map[string]any
	if err := json.Unmarshal(encoded, &policy); err != nil {
		return err
	}
	if got := strings.Join(sortedObjectKeys(policy), ","); got != "aliases,contentCapture,fallback,id,protocols,requestLimits,routes,version,workspaceId" {
		return fmt.Errorf("POC policy fields=%s", got)
	}
	if atString(policy, "id") != "11111111-1111-4111-8111-111111111111" || atNumber(policy, "version") != 1 || atString(policy, "workspaceId") != "22222222-2222-4222-8222-222222222222" || at(policy, "contentCapture") != false {
		return errorsNew("POC policy identity or capture semantics changed")
	}
	protocols := stringSlice(at(policy, "protocols"))
	wantProtocols := []string{"openai-responses", "openai-chat", "anthropic-messages"}
	if strings.Join(protocols, ",") != strings.Join(wantProtocols, ",") {
		return fmt.Errorf("POC source protocols=%v want=%v", protocols, wantProtocols)
	}
	routes, ok := at(policy, "routes").([]any)
	if !ok || len(routes) != 2 {
		return errorsNew("POC policy must have exactly local and cloud routes")
	}
	local, localOK := routes[0].(map[string]any)
	cloud, cloudOK := routes[1].(map[string]any)
	if !localOK || !cloudOK {
		return errorsNew("POC policy routes must be objects")
	}
	wantRouteFields := "acceptedDataClasses,capabilities,costClass,credentialRef,dataBoundary,destinationClass,destinationProtocol,endpoint,healthTimeoutMs,id,model,sourceProtocols"
	if strings.Join(sortedObjectKeys(local), ",") != wantRouteFields || strings.Join(sortedObjectKeys(cloud), ",") != wantRouteFields {
		return errorsNew("POC route fields changed")
	}
	for index, route := range []map[string]any{local, cloud} {
		if strings.Join(stringSlice(route["sourceProtocols"]), ",") != strings.Join(wantProtocols, ",") {
			return fmt.Errorf("POC route %d sourceProtocols changed", index)
		}
	}
	if atString(local, "id") != "33333333-3333-4333-8333-333333333333" || atString(local, "destinationClass") != "local_node" || atString(local, "destinationProtocol") != "openai-chat" || atString(local, "model") != "qwen3.8" || atString(local, "dataBoundary") != "local" || atString(local, "costClass") != "local" || atString(local, "endpoint", "resolvedAddressPolicy") != "loopback_only" {
		return errorsNew("POC local Qwen route semantics changed")
	}
	if atString(cloud, "id") != "44444444-4444-4444-8444-444444444444" || atString(cloud, "destinationClass") != "provider" || atString(cloud, "destinationProtocol") != "openai-responses" || atString(cloud, "model") != "gpt-5.4" || atString(cloud, "dataBoundary") != "external" || atString(cloud, "costClass") != "metered_high" || atString(cloud, "endpoint", "scheme") != "https" || atString(cloud, "endpoint", "resolvedAddressPolicy") != "public_unicast_only" {
		return errorsNew("POC cloud fallback route semantics changed")
	}
	aliases, ok := at(policy, "aliases").(map[string]any)
	if !ok || len(aliases) != 4 {
		return errorsNew("POC alias matrix changed")
	}
	company, ok := aliases["company-assistant"].(map[string]any)
	if !ok || strings.Join(stringSlice(company["routeIds"]), ",") != "33333333-3333-4333-8333-333333333333,44444444-4444-4444-8444-444444444444" || atString(company, "dataClass") != "company" {
		return errorsNew("POC local-first company route order changed")
	}
	if atNumber(policy, "fallback", "maxAttempts") != 2 || strings.Join(stringSlice(at(policy, "fallback", "allowedBoundaryTransitions")), ",") != "local_to_external" {
		return errorsNew("POC fallback semantics changed")
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
	if atNumber(document, "properties", "environment", "minItems") != 5 || atNumber(document, "properties", "environment", "maxItems") != 5 || lenArray(at(document, "properties", "environment", "allOf")) != 5 || atNumber(document, "properties", "listener", "properties", "pid", "minimum") != 1 {
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
	if atNumber(document, "properties", "environment", "minItems") != 5 || atNumber(document, "properties", "environment", "maxItems") != 5 || lenArray(at(document, "properties", "environment", "allOf")) != 5 || atString(document, "properties", "listener", "properties", "listenerKeyFingerprint", "pattern") != "^sha256:[0-9a-f]{64}$" || atString(document, "properties", "listener", "properties", "address", "pattern") != "^(127\\.0\\.0\\.1|\\[::1\\]):[0-9]{1,5}$" {
		return errorsNew("receipt environment/listener binding changed")
	}
	return nil
}

func validateEvent(document map[string]any) error {
	if atNumber(document, "properties", "attempt", "minimum") != 1 || atNumber(document, "properties", "attempt", "maximum") != 2 || at(document, "properties", "usage", "additionalProperties") != false || strings.Join(sortedStrings(at(document, "properties", "usage", "required")), ",") != "inputTokens,outputTokens" || lenArray(at(document, "oneOf")) != 8 {
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
	if lenArray(at(request, "$defs", "block", "oneOf")) != 3 {
		return errorsNew("normalized request block union changed")
	}
	if lenArray(at(documents["normalized-response.schema.json"], "properties", "blocks", "items", "oneOf")) != 2 {
		return errorsNew("normalized response block union changed")
	}
	stream := documents["normalized-stream-event.schema.json"]
	if atString(stream, "properties", "error", "$ref") != "normalized-error.schema.json" {
		return errorsNew("normalized stream error ref changed")
	}
	if lenArray(at(stream, "oneOf")) != 7 {
		return errorsNew("normalized stream discriminated union changed")
	}
	if strings.Join(sortedStrings(at(stream, "properties", "usage", "required")), ",") != "inputTokens,outputTokens" {
		return errorsNew("normalized stream usage fields changed")
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
func stringSlice(value any) []string {
	values, _ := value.([]any)
	result := make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if !ok {
			return nil
		}
		result = append(result, text)
	}
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

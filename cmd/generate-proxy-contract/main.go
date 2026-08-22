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
	"activation-journal.schema.json": "ad321497bd9376a2a8f9aa7a06be3bd6dab88ce2334f6c9ed37b5752c1b52289",
	"event.schema.json":              "7458b020e48b0d346503b2f054751d1a85fcd534c4ab3978150e59a769acbf26",
	"policy.schema.json":             "ee4d021e057f16fb5748023dad25c33dc92fbac5e30269302268f0893f097164",
}

var requiredFields = map[string][]string{
	"activation-journal.schema.json": {"activationId", "binary", "checksum", "createdAt", "environment", "generation", "listener", "nonce", "ownerUid", "policy", "rollbackActions", "schemaVersion", "state", "updatedAt"},
	"event.schema.json":              {"activationId", "attempt", "cursor", "destinationClass", "eventId", "latencyMs", "logicalRequestId", "modelAlias", "outcome", "protocol", "reasonCode", "routeId", "timestamp", "type"},
	"policy.schema.json":             {"aliases", "contentCapture", "fallback", "id", "protocols", "requestLimits", "routes", "version", "workspaceId"},
}

var propertyFields = map[string][]string{
	"activation-journal.schema.json": {"activationId", "binary", "ca", "checksum", "createdAt", "environment", "generation", "listener", "nonce", "ownerUid", "policy", "rollbackActions", "schemaVersion", "state", "updatedAt"},
	"event.schema.json":              {"activationId", "attempt", "cursor", "destinationClass", "eventId", "latencyMs", "logicalRequestId", "modelAlias", "outcome", "protocol", "reasonCode", "routeId", "timestamp", "type", "usage"},
	"policy.schema.json":             {"aliases", "contentCapture", "fallback", "id", "protocols", "requestLimits", "routes", "version", "workspaceId"},
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
func errorsNew(value string) error { return generatedError(value) }

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
	if err := validatePolicy(documents["policy.schema.json"]); err != nil { return err }
	if err := validateJournal(documents["activation-journal.schema.json"]); err != nil { return err }
	if err := validateEvent(documents["event.schema.json"]); err != nil { return err }
	for _, tag := range []string{"workspaceId", "contentCapture", "rollbackActions", "listenerKeyFingerprint", "logicalRequestId", "destinationClass", "inputTokens", "outputTokens"} {
		if !strings.Contains(template, `json:"`+tag) { return fmt.Errorf("generated template lacks JSON field %s", tag) }
	}
	return nil
}

func validatePolicy(document map[string]any) error {
	if at(document, "properties", "contentCapture", "const") != false { return errorsNew("policy contentCapture must be const false") }
	if atNumber(document, "properties", "fallback", "properties", "maxAttempts", "minimum") != 1 || atNumber(document, "properties", "fallback", "properties", "maxAttempts", "maximum") != 2 { return errorsNew("policy fallback maxAttempts must be 1..2") }
	if atNumber(document, "properties", "aliases", "patternProperties", "^[a-zA-Z0-9._:-]{1,128}$", "maxItems") != 2 { return errorsNew("policy aliases must contain at most two route refs") }
	if atString(document, "properties", "aliases", "patternProperties", "^[a-zA-Z0-9._:-]{1,128}$", "items", "format") != "uuid" { return errorsNew("policy alias route refs must be UUIDs") }
	if got := sortedStrings(at(document, "properties", "protocols", "items", "enum")); strings.Join(got, ",") != "anthropic-messages,openai-chat,openai-responses" { return fmt.Errorf("policy protocols changed: %v", got) }
	if got := sortedStrings(at(document, "properties", "routes", "items", "required")); strings.Join(got, ",") != "baseHostname,capabilities,costClass,credentialRef,dataBoundary,destinationClass,destinationProtocol,healthTimeoutMs,id,model,sourceProtocol" { return fmt.Errorf("policy route fields changed: %v", got) }
	return nil
}

func validateJournal(document map[string]any) error {
	if atString(document, "properties", "checksum", "pattern") != "^sha256:[0-9a-f]{64}$" || atString(document, "properties", "policy", "properties", "digest", "pattern") != "^sha256:[0-9a-f]{64}$" { return errorsNew("journal checksum/digest patterns changed") }
	if got := sortedStrings(at(document, "properties", "state", "enum")); strings.Join(got, ",") != "active,deactivating,prepared,publishing,recovery_required" { return fmt.Errorf("journal states changed: %v", got) }
	if got := sortedStrings(at(document, "properties", "environment", "items", "properties", "name", "enum")); strings.Join(got, ",") != "ANTHROPIC_API_KEY,ANTHROPIC_BASE_URL,OPENAI_API_KEY,OPENAI_BASE_URL" { return fmt.Errorf("journal environment names changed: %v", got) }
	if atNumber(document, "properties", "environment", "maxItems") != 16 || atNumber(document, "properties", "listener", "properties", "pid", "minimum") != 1 { return errorsNew("journal environment/listener bounds changed") }
	if got := sortedStrings(at(document, "properties", "environment", "items", "properties", "rollbackAction", "enum")); strings.Join(got, ",") != "remove,restore" { return fmt.Errorf("journal rollback actions changed: %v", got) }
	return nil
}

func validateEvent(document map[string]any) error {
	if atNumber(document, "properties", "attempt", "minimum") != 1 || atNumber(document, "properties", "attempt", "maximum") != 2 || at(document, "properties", "usage", "additionalProperties") != false { return errorsNew("event attempt/usage bounds changed") }
	for _, forbidden := range []string{"authorization", "body", "content", "cookie", "credential", "headers", "messages", "prompt", "response", "tools"} {
		if at(document, "properties", forbidden) != nil { return fmt.Errorf("event schema contains forbidden content field %s", forbidden) }
	}
	if got := sortedStrings(at(document, "properties", "protocol", "enum")); strings.Join(got, ",") != "anthropic-messages,openai-chat,openai-responses" { return fmt.Errorf("event protocols changed: %v", got) }
	return nil
}

func render(digests map[string]string) []byte {
	_, rest, found := bytes.Cut(contractTemplate, []byte("\n")); if !found { rest = contractTemplate }
	header := "// Code generated from packages/contracts/proxy; DO NOT EDIT.\n" +
		"// activation-journal.schema.json SHA256: " + digests["activation-journal.schema.json"] + "\n" +
		"// event.schema.json SHA256: " + digests["event.schema.json"] + "\n" +
		"// policy.schema.json SHA256: " + digests["policy.schema.json"] + "\n"
	return append([]byte(header), rest...)
}

func sortedObjectKeys(value any) []string { object, _ := value.(map[string]any); keys := make([]string, 0, len(object)); for key := range object { keys = append(keys, key) }; sort.Strings(keys); return keys }
func sortedStrings(value any) []string { values, _ := value.([]any); result := make([]string, 0, len(values)); for _, value := range values { if text, ok := value.(string); ok { result = append(result, text) } }; sort.Strings(result); return result }
func atString(root any, path ...string) string { value, _ := at(root, path...).(string); return value }
func atNumber(root any, path ...string) float64 { value, _ := at(root, path...).(float64); return value }
func at(root any, path ...string) any { current := root; for _, part := range path { object, ok := current.(map[string]any); if !ok { return nil }; current = object[part] }; return current }

func repositoryRoot() (string, error) { _, source, _, ok := runtime.Caller(0); if !ok { return "", errorsNew("locate generator source") }; root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..")); if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil { return "", err }; return root, nil }
func fatalIf(err error) { if err != nil { fmt.Fprintln(os.Stderr, "generate-proxy-contract:", err); os.Exit(1) } }

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

const harnessDigest = "06c5fb15126d18e51164fc3585918506bbcafdec3c88e2707ccbbc000d70b3b6"
const scopeDigest = "ed5d32806db5f678463ed9066d83c388b749bf2e46641882fcc661bb2c04a31e"

func main() {
	check := flag.Bool("check", false, "fail if generated harness worker contracts differ")
	flag.Parse()
	root, err := repositoryRoot()
	fatalIf(err)
	harness := readPinned(filepath.Join(root, "packages/contracts/harness-worker.schema.json"), harnessDigest)
	scope := readPinned(filepath.Join(root, "packages/contracts/proxy/workload-scope.schema.json"), scopeDigest)
	fatalIf(validateSchemas(harness, scope))
	generated, err := format.Source(render())
	fatalIf(err)
	output := filepath.Join(root, "internal/harnessworker/contracts.gen.go")
	if *check {
		current, err := os.ReadFile(output)
		fatalIf(err)
		if !bytes.Equal(current, generated) {
			fatalIf(fmt.Errorf("generated harness worker contracts are stale; run 'go generate ./internal/harnessworker'"))
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

func readPinned(path, pinned string) map[string]any {
	encoded, err := os.ReadFile(path)
	fatalIf(err)
	digest := sha256.Sum256(encoded)
	if actual := hex.EncodeToString(digest[:]); actual != pinned {
		fatalIf(fmt.Errorf("%s digest=%s pinned=%s", filepath.Base(path), actual, pinned))
	}
	var document map[string]any
	fatalIf(json.Unmarshal(encoded, &document))
	return document
}

func validateSchemas(harness, scope map[string]any) error {
	for name, document := range map[string]map[string]any{"harness-worker": harness, "workload-scope": scope} {
		if stringAt(document, "$schema") != "https://json-schema.org/draft/2020-12/schema" || stringAt(document, "type") != "object" || valueAt(document, "additionalProperties") != false {
			return fmt.Errorf("%s must be a closed draft-2020-12 object", name)
		}
	}
	if joinedKeys(valueAt(harness, "properties")) != "schemaVersion,scope,type" || joinedStrings(valueAt(harness, "required")) != "schemaVersion,scope,type" {
		return fmt.Errorf("harness worker fields changed")
	}
	if stringAt(harness, "properties", "schemaVersion", "const") != "blazn.dev/harness-worker/v1alpha1" || stringAt(harness, "properties", "type", "const") != "execute" || stringAt(harness, "properties", "scope", "$ref") != "proxy/workload-scope.schema.json" {
		return fmt.Errorf("harness worker identity changed")
	}
	wantScopeFields := "agentVersionDigest,agentVersionId,expiresAt,harnessExecutableDigest,harnessProfileDigest,harnessProfileId,harnessVersionDigest,harnessVersionId,listenerCredentialRef,listenerTokenFingerprint,operationId,projectId,protocol,routeId,routeVersion,runId,sandboxId,workspaceId"
	if joinedKeys(valueAt(scope, "properties")) != wantScopeFields || joinedStrings(valueAt(scope, "required")) != wantScopeFields {
		return fmt.Errorf("workload scope fields changed")
	}
	if joinedStrings(valueAt(scope, "properties", "protocol", "enum")) != "anthropic-messages,openai-chat,openai-responses" {
		return fmt.Errorf("route binding changed")
	}
	if numberAt(scope, "properties", "routeVersion", "minimum") != 1 || numberAt(scope, "properties", "routeVersion", "maximum") != 9007199254740991 {
		return fmt.Errorf("route version bounds changed")
	}
	if stringAt(scope, "properties", "expiresAt", "format") != "date-time" || stringAt(scope, "properties", "listenerTokenFingerprint", "$ref") != "#/$defs/digest" || !strings.HasPrefix(stringAt(scope, "properties", "listenerCredentialRef", "pattern"), "^listener-token://") {
		return fmt.Errorf("expiry/listener binding changed")
	}
	if stringAt(scope, "$defs", "digest", "pattern") != "^sha256:[0-9a-f]{64}$" || stringAt(scope, "$defs", "id", "pattern") != "^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$" {
		return fmt.Errorf("identity/fingerprint patterns changed")
	}
	for _, document := range []map[string]any{harness, scope} {
		if stringAt(document, "x-blazn-secret-boundary", "listenerTokenDelivery") != "out-of-band" {
			return fmt.Errorf("listener token delivery must remain out-of-band")
		}
		forbidden := joinedStrings(valueAt(document, "x-blazn-secret-boundary", "forbiddenKeys"))
		for _, key := range []string{"apiKey", "authorization", "credential", "listenerToken", "password", "privateKey", "secret", "token"} {
			if !strings.Contains(","+forbidden+",", ","+key+",") {
				return fmt.Errorf("secret boundary lacks %s", key)
			}
		}
	}
	template := string(contractTemplate)
	for _, required := range []string{"WorkloadScopeMaxLifetime = 24 * time.Hour", "ListenerTokenFingerprint(token []byte)", "len(token) > contractMaxListenerTokenBytes", "json:\"harnessExecutableDigest\"", "json:\"routeId\"", "json:\"expiresAt\""} {
		if !strings.Contains(template, required) {
			return fmt.Errorf("generated template lacks pinned semantic %q", required)
		}
	}
	return nil
}

func render() []byte {
	value := strings.ReplaceAll(string(contractTemplate), "{{HARNESS_DIGEST}}", harnessDigest)
	value = strings.ReplaceAll(value, "{{SCOPE_DIGEST}}", scopeDigest)
	return []byte(value)
}

func valueAt(document map[string]any, path ...string) any {
	var value any = document
	for _, key := range path {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = object[key]
	}
	return value
}
func stringAt(document map[string]any, path ...string) string {
	value, _ := valueAt(document, path...).(string)
	return value
}
func numberAt(document map[string]any, path ...string) float64 {
	value, _ := valueAt(document, path...).(float64)
	return value
}
func joinedKeys(value any) string {
	object, _ := value.(map[string]any)
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}
func joinedStrings(value any) string {
	values, _ := value.([]any)
	stringsOnly := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			stringsOnly = append(stringsOnly, text)
		}
	}
	sort.Strings(stringsOnly)
	return strings.Join(stringsOnly, ",")
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

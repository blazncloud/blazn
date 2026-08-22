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
	"strconv"
	"strings"
)

//go:embed sandbox.gen.go.tmpl
var clientTemplate []byte

const (
	openAPISHA256  = "996b5143c4e99f8a356c1d7da45570b48859d277c3267ef7140e6536123a712e"
	templateSHA256 = "29c0779892c4fb7bf25682bf39d3721649602b3927b8b9764b00398a1c784acf"
	cliSHA256      = "772cea3cb6a3fc9247e1aa85330a2d3de56dfd63cecb06aec876390e22f79808"
)

type source struct {
	path, digest string
	document     map[string]any
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
	if len(paths) != 12 {
		return fmt.Errorf("sandbox paths changed: got %d want 12", len(paths))
	}
	wantOps := []string{"createSandboxTemplate", "listSandboxTemplates", "getSandboxTemplate", "replaceSandboxTemplateDraft", "publishSandboxTemplateVersion", "listSandboxTemplateVersions", "getSandboxTemplateVersion", "createSandbox", "listSandboxes", "getSandbox", "streamSandboxEvents", "createSandboxOperation", "createSandboxAccessGrant", "executeSandboxGrant", "uploadSandboxGrantFile", "downloadSandboxGrantFile"}
	seen := map[string]bool{}
	for _, pathValue := range paths {
		methods, _ := pathValue.(map[string]any)
		for _, operationValue := range methods {
			operation, _ := operationValue.(map[string]any)
			seen[atString(operation, "operationId")] = true
			if atString(operation, "responses", "default", "$ref") != "#/components/responses/SandboxError" {
				return fmt.Errorf("sandbox operation %q lacks common error response", atString(operation, "operationId"))
			}
		}
	}
	for _, op := range wantOps {
		if !seen[op] {
			return fmt.Errorf("sandbox operation missing: %s", op)
		}
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
	for _, marker := range []string{"func CanonicalSandboxTemplateDigest(", `"Blazn-Grant "+grantToken`, `"Idempotency-Key",idempotencyKey`, `"Last-Event-ID",lastEventID`, "SandboxMaxFileBytes", "ApprovedNonSensitive"} {
		if !strings.Contains(template, marker) {
			return fmt.Errorf("sandbox client template lacks marker %q", marker)
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

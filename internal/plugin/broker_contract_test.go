package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestBrokerRequestContractIsClosedScopedAndSecretFree(t *testing.T) {
	root := filepath.Join("..", "..")
	encoded, err := os.ReadFile(filepath.Join(root, "packages", "contracts", "plugin-broker-request.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	if document["$id"] != "https://blazn.benpelo.com/contracts/plugin-broker-request-v1.schema.json" {
		t.Fatalf("schema id=%v", document["$id"])
	}
	methods := collectBrokerMethodConstants(document)
	want := []string{"artifact.get", "artifact.list", "artifact.upload.begin", "broker.describe", "project.get", "run.cancel", "run.create", "run.get", "run.list", "run.synthetic.complete", "run.synthetic.progress"}
	if !reflect.DeepEqual(methods, want) {
		t.Fatalf("methods=%v want=%v", methods, want)
	}
	lower := strings.ToLower(string(encoded))
	for _, forbidden := range []string{"accesstoken", "refreshtoken", "authorization", "apikey", "credential", "objectkey", "signedurl", "workspaceid", "projectid", "userid", "apiorigin", "placement"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("broker request contract contains forbidden authority field %q", forbidden)
		}
	}
	if strings.Count(lower, `"additionalproperties":false`) < len(want) {
		t.Fatal("broker request variants are not closed")
	}
}

func TestBrokerResponseContractIsClosedAndSchemaNegotiated(t *testing.T) {
	encoded, err := os.ReadFile(filepath.Join("..", "..", "packages", "contracts", "plugin-broker-response.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if json.Unmarshal(encoded, &document) != nil {
		t.Fatal("response schema is not JSON")
	}
	text := strings.ToLower(string(encoded))
	for _, schema := range []string{"broker-description/v1", "project-envelope/v1", "run-envelope/v1", "run-list/v1", "artifact-envelope/v1", "artifact-list/v1", "progress-ack/v1", "artifact-upload-ready/v1"} {
		if !strings.Contains(text, schema) {
			t.Fatalf("response schema lacks %q", schema)
		}
	}
	for _, forbidden := range []string{"accesstoken", "refreshtoken", "authorization", "credential", "objectkey", "signedurl"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("response envelope contains forbidden field %q", forbidden)
		}
	}
}

func TestBrokerDocumentationFreezesFramingAndProofBoundary(t *testing.T) {
	encoded, err := os.ReadFile(filepath.Join("..", "..", "docs", "plugin-broker-contract.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, marker := range []string{"16-byte big-endian header", "BLAZN_PLUGIN_BROKER_FD=3", "proofClass=synthetic", "sole two-response exchange", "artifact-upload-ready/v1", "terminal `artifact-envelope/v1`", "exact digest/size match", "Partial or cancelled uploads are removed"} {
		if !strings.Contains(text, marker) {
			t.Fatalf("broker documentation lacks %q", marker)
		}
	}
}

func collectBrokerMethodConstants(value any) []string {
	seen := map[string]bool{}
	var visit func(any)
	visit = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			if constant, ok := typed["const"].(string); ok && (strings.Contains(constant, ".") || constant == "broker.describe") {
				seen[constant] = true
			}
			for _, child := range typed {
				visit(child)
			}
		case []any:
			for _, child := range typed {
				visit(child)
			}
		}
	}
	visit(value)
	methods := make([]string, 0, len(seen))
	for method := range seen {
		methods = append(methods, method)
	}
	sort.Strings(methods)
	return methods
}

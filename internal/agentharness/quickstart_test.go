package agentharness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blazncloud/blazn/internal/client"
)

func quickstartFixture(t *testing.T, file, key string) client.JSONDocument {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "packages", "contracts", "testdata", "harness", file))
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	var bundle map[string]client.JSONDocument
	if err := decoder.Decode(&bundle); err != nil {
		t.Fatal(err)
	}
	return bundle[key]
}

// The control API recomputes every document digest; quickstart must produce
// exactly the digests the reviewed fixtures carry.
func TestDocumentDigestMatchesTheReviewedFixtures(t *testing.T) {
	for _, test := range []struct {
		file, key, domain string
		excluded          []string
	}{
		{"hermes-profile.json", "version", "blazn-harness-version-v1", []string{"digest"}},
		{"hermes-profile.json", "profile", "blazn-harness-profile-v1", []string{"digest", "resourceVersion"}},
		{"claude-profile.json", "profile", "blazn-harness-profile-v1", []string{"digest", "resourceVersion"}},
		{"agent-good.json", "version", "blazn-agent-version-v1", []string{"digest"}},
	} {
		document := quickstartFixture(t, test.file, test.key)
		want, _ := document["digest"].(string)
		if got := documentDigest(test.domain, document, test.excluded...); want == "" || got != want {
			t.Errorf("%s %s digest = %s, want %s", test.file, test.key, got, want)
		}
	}
}

func TestCanonicalStringsMatchJavaScript(t *testing.T) {
	var b strings.Builder
	writeCanonical(&b, map[string]any{"z": []any{1, true, nil, "a\"b\\c\n\t\u0001<>& é"}, "a": map[string]any{}})
	if got, want := b.String(), "{\"a\":{},\"z\":[1,true,null,\"a\\\"b\\\\c\\n\\t\\u0001<>& é\"]}"; got != want {
		t.Fatalf("canonical = %s, want %s", got, want)
	}
}

func TestReferenceHarnessDocumentsValidateAgainstTheSchemas(t *testing.T) {
	version := referenceHarnessVersionDocument("11111111-1111-4111-8111-111111111111", "not-a-commit")
	version["digest"] = documentDigest("blazn-harness-version-v1", version, "digest")
	if err := ValidateDocument("harness-version", version); err != nil {
		t.Fatalf("reference harness version is invalid: %v", err)
	}
	if commit := version["provenance"].(map[string]any)["commit"]; commit != strings.Repeat("0", 40) {
		t.Fatalf("an unknown build commit must fall back to zeros, got %v", commit)
	}
	options := QuickstartOptions{Name: "helper", Template: "coding-agent@v1", Repository: "https://github.com/blazncloud/blazn.git", Commit: strings.Repeat("a", 40),
		Instructions: "Help.", ModelRouteID: "0a000000-0000-4000-8000-000000000001", ModelRouteVersion: 1, RequestID: "quickstart-1"}
	if err := options.validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*QuickstartOptions){
		func(o *QuickstartOptions) { o.Name = "Helper" },
		func(o *QuickstartOptions) { o.Template = "coding-agent" },
		func(o *QuickstartOptions) { o.Repository = "https://user@github.com/x/y.git" },
		func(o *QuickstartOptions) { o.Commit = "abc" },
		func(o *QuickstartOptions) { o.Instructions = "" },
		func(o *QuickstartOptions) { o.ModelRouteID = "route" },
		func(o *QuickstartOptions) { o.RequestID = "short" },
	} {
		invalid := options
		mutate(&invalid)
		if invalid.validate() == nil {
			t.Fatalf("invalid options accepted: %+v", invalid)
		}
	}
}

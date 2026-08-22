package sandbox

import (
	"os"
	"strings"
	"testing"

	"github.com/blazncloud/blazn/internal/client"
)

func TestValidateTemplateAndJCSIdentity(t *testing.T) {
	manifest, err := os.ReadFile("../../packages/contracts/testdata/sandbox/template-good.json")
	if err != nil {
		t.Fatal(err)
	}
	result := ValidateTemplate(manifest)
	if !result.Valid || result.ManifestDigest == nil || len(result.Errors) != 0 {
		t.Fatalf("validation = %+v", result)
	}
	expected, _, err := client.CanonicalSandboxTemplateDigest(manifest)
	if err != nil || *result.ManifestDigest != expected {
		t.Fatalf("digest=%v expected=%q err=%v", result.ManifestDigest, expected, err)
	}
	a, _ := os.ReadFile("../../packages/contracts/testdata/sandbox/canonical-vector-a.json")
	b, _ := os.ReadFile("../../packages/contracts/testdata/sandbox/canonical-vector-b.json")
	da, _, errA := client.CanonicalSandboxTemplateDigest(a)
	db, _, errB := client.CanonicalSandboxTemplateDigest(b)
	if errA != nil || errB != nil || da != db {
		t.Fatalf("canonical vectors differ: %q %q %v %v", da, db, errA, errB)
	}
}

func TestValidateTemplateRejectsSchemaAndCrossItemViolations(t *testing.T) {
	manifest, _ := os.ReadFile("../../packages/contracts/testdata/sandbox/template-good.json")
	tests := []struct{ name, old, replacement, want string }{
		{"unknown", `"metadata": {`, `"unexpected":true,"metadata": {`, "unknown field"},
		{"privileged", `"platform": "linux",`, `"platform": "linux", "privileged": true,`, "unknown field"},
		{"duplicate architecture", `"architecture": "arm64"`, `"architecture": "amd64"`, "architecture must be unique"},
		{"parent segment", `/workspace/src/blazn`, `/workspace/src/../blazn`, "confined"},
		{"missing required boolean", `, "writable": true`, ``, "writable is required"},
		{"duplicate property", `"description": "Pinned`, `"version":"duplicate","description": "Pinned`, "duplicate object property"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := strings.Replace(string(manifest), test.old, test.replacement, 1)
			result := ValidateTemplate([]byte(changed))
			if result.Valid || result.ManifestDigest != nil || !strings.Contains(strings.Join(result.Errors, " "), test.want) {
				t.Fatalf("validation = %+v", result)
			}
		})
	}
}

func TestValidateTemplateRejectsTrailingDocument(t *testing.T) {
	manifest, _ := os.ReadFile("../../packages/contracts/testdata/sandbox/template-good.json")
	result := ValidateTemplate(append(manifest, []byte(` {}`)...))
	if result.Valid || !strings.Contains(strings.Join(result.Errors, " "), "exactly one") {
		t.Fatalf("validation = %+v", result)
	}
}

func TestReadTemplateFileRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := dir + "/target"
	link := dir + "/link"
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTemplateFile(link); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

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
	if result.Valid || result.ManifestDigest != nil || len(result.Errors) == 0 {
		t.Fatalf("validation = %+v", result)
	}
}

func TestImmutableOCIReferenceUsesCanonicalRegistryAndRepositoryIdentity(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, value := range []string{
		"registry.example.test/blazn/sandbox@" + digest,
		"registry.example.test:80/blazn/sandbox@" + digest,
		"registry.example.test:443/blazn/sandbox__image@" + digest,
		"registry.example.test/blazn/sandbox--image@" + digest,
		"registry.example.test/" + strings.Repeat("a", 255) + "@" + digest,
		"localhost:5000/blazn/sandbox@" + digest,
		"127.0.0.1:5000/blazn/sandbox@" + digest,
	} {
		if !IsImmutableOCIReference(value) {
			t.Errorf("canonical reference %q was rejected", value)
		}
	}
	for _, value := range []string{
		"blazn/sandbox@" + digest,
		"registry.example.test/blazn/sandbox:latest@" + digest,
		"registry.example.test:0/blazn/sandbox@" + digest,
		"registry.example.test:65536/blazn/sandbox@" + digest,
		"registry.example.test:080/blazn/sandbox@" + digest,
		"bad-.example.test/blazn/sandbox@" + digest,
		"registry.example.test/blazn//sandbox@" + digest,
		"registry.example.test/" + strings.Repeat("a", 256) + "@" + digest,
		"REGISTRY.example.test/blazn/sandbox@" + digest,
		"registry.example.test/blazn/sandbox@sha256:" + strings.Repeat("A", 64),
	} {
		if IsImmutableOCIReference(value) {
			t.Errorf("non-canonical reference %q was accepted", value)
		}
	}
}

func TestValidateTemplateNullUTF8URLAndCodePointBoundaries(t *testing.T) {
	manifest, _ := os.ReadFile("../../packages/contracts/testdata/sandbox/template-good.json")
	t.Run("null optional array", func(t *testing.T) {
		changed := strings.Replace(string(manifest), `"repositories": [{"name": "source", "url": "https://github.com/blazncloud/blazn.git", "destination": "/workspace/src/blazn", "writable": true}],`, `"repositories": null,`, 1)
		result := ValidateTemplate([]byte(changed))
		if result.Valid || !strings.Contains(strings.Join(result.Errors, " "), "not null") {
			t.Fatalf("validation=%+v", result)
		}
	})
	t.Run("invalid utf8", func(t *testing.T) {
		changed := append([]byte(nil), manifest...)
		changed[len(changed)/2] = 0xff
		result := ValidateTemplate(changed)
		if result.Valid || !strings.Contains(strings.Join(result.Errors, " "), "UTF-8") {
			t.Fatalf("validation=%+v", result)
		}
	})
	t.Run("at in repository path", func(t *testing.T) {
		changed := strings.Replace(string(manifest), "https://github.com/blazncloud/blazn.git", "https://github.com/blazncloud/blazn@release.git", 1)
		if result := ValidateTemplate([]byte(changed)); !result.Valid {
			t.Fatalf("validation=%+v", result)
		}
	})
	t.Run("query rejected", func(t *testing.T) {
		changed := strings.Replace(string(manifest), "https://github.com/blazncloud/blazn.git", "https://github.com/blazncloud/blazn.git?ref=main", 1)
		if result := ValidateTemplate([]byte(changed)); result.Valid {
			t.Fatal("query URL accepted")
		}
	})
	t.Run("unicode code points", func(t *testing.T) {
		valid := strings.Replace(string(manifest), "Pinned non-sensitive Phase 5 bootstrap fixture", strings.Repeat("é", 1024), 1)
		if result := ValidateTemplate([]byte(valid)); !result.Valid {
			t.Fatalf("1024 code points invalid: %+v", result)
		}
		invalid := strings.Replace(string(manifest), "Pinned non-sensitive Phase 5 bootstrap fixture", strings.Repeat("é", 1025), 1)
		if result := ValidateTemplate([]byte(invalid)); result.Valid {
			t.Fatal("1025 code points accepted")
		}
	})
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

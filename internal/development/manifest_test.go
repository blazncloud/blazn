package development

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateDevelopmentProjectAndRejectAttacks(t *testing.T) {
	good, err := os.ReadFile("../../examples/coding-agent/blazn.yaml")
	if err != nil {
		t.Fatal(err)
	}
	result, manifest := Validate(good)
	if !result.Valid || result.ManifestDigest == nil || manifest == nil {
		t.Fatalf("valid result=%#v", result)
	}
	for name, candidate := range map[string][]byte{
		"duplicate": []byte(`{"schemaVersion":"blazn.dev/project/v1alpha1","schemaVersion":"blazn.dev/project/v1alpha1"}`),
		"unknown":   []byte(strings.Replace(string(good), `"projectId":`, `"unexpected":true,"projectId":`, 1)),
		"shell":     []byte(strings.Replace(string(good), `["node", "--test"`, `["sh", "--test"`, 1)),
		"secret":    []byte(strings.Replace(string(good), `"test/context-identity.test.mjs"`, `"--api-key=secret"`, 1)),
		"path":      []byte(strings.Replace(string(good), `"examples/coding-agent/Dockerfile"`, `"../Dockerfile"`, 1)),
	} {
		if got, _ := Validate(candidate); got.Valid || len(got.Errors) == 0 {
			t.Fatalf("%s attack accepted: %#v", name, got)
		}
	}
}

func TestValidateRejectsSignedURLArgumentsAndInvalidRepositoryAuthorities(t *testing.T) {
	good, err := os.ReadFile("../../examples/coding-agent/blazn.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for name, argument := range map[string]string{
		"azure sig":              "https://account.blob.core.windows.net/container/object?sv=2024-11-04&sig=secret",
		"generic signature":      "--download=https://objects.example.test/item?signature=secret",
		"aws signature":          "https://bucket.s3.example.test/item?X-Amz-Signature=secret",
		"normalized aws variant": "https://bucket.s3.example.test/item?x_amz_signature=secret",
		"encoded aws variant":    "https://bucket.s3.example.test/item?X%2dAmz%2dSignature=secret",
		"fully encoded aws URL":  url.QueryEscape("https://bucket.s3.example.test/item?X-Amz-Signature=secret"),
		"google signature":       "log=https://storage.googleapis.test/item?X-Goog-Signature=secret",
		"generic URI userinfo":   "http://user:password@example.test/item",
	} {
		t.Run(name, func(t *testing.T) {
			candidate := []byte(strings.Replace(string(good), `"test/context-identity.test.mjs"`, `"`+argument+`"`, 1))
			if result, _ := Validate(candidate); result.Valid {
				t.Fatalf("signed URL argument accepted: %s", argument)
			}
		})
	}

	for name, repositoryURL := range map[string]string{
		"triple slash":          "https:///blazncloud/blazn.git",
		"empty hostname":        "https://:443/blazncloud/blazn.git",
		"malformed hostname":    "https://github..com/blazncloud/blazn.git",
		"userinfo authority":    "https://github.com@evil.test/blazncloud/blazn.git",
		"percent encoded name":  "https://github.com/blazncloud/bl%61zn.git",
		"percent encoded slash": "https://github.com/blazncloud%2Fblazn.git",
		"trailing slash":        "https://github.com/blazncloud/blazn.git/",
		"extra leading slash":   "https://github.com//blazncloud/blazn.git",
		"space in name":         "https://github.com/blazncloud/blazn repo.git",
		"third path segment":    "https://github.com/blazncloud/blazn/extra",
	} {
		t.Run(name, func(t *testing.T) {
			candidate := []byte(strings.Replace(string(good), "https://github.com/blazncloud/blazn.git", repositoryURL, 1))
			if result, _ := Validate(candidate); result.Valid {
				t.Fatalf("invalid repository URL accepted: %s", repositoryURL)
			}
		})
	}
}

func TestReadFileRejectsSymlinkAndWritableManifest(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "manifest.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(link); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := os.Chmod(target, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(target); err == nil {
		t.Fatal("group/world writable manifest accepted")
	}
}

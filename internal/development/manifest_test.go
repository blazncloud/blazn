package development

import (
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

package plugin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSocialCatalogPinsV2AndRejectsRetiredV1Signature(t *testing.T) {
	sshKeygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen unavailable")
	}
	definition, ok := DefaultCatalog().Plugin("social")
	if !ok {
		t.Fatal("social catalog definition missing")
	}
	fields := strings.Fields(definition.AllowedSigner)
	if len(fields) < 5 || fields[0] != definition.SigningIdentity || fields[1] != `namespaces="blazn-social-release"` || fields[2] != "ssh-ed25519" || fields[4] != "blazn-social-release-v2" {
		t.Fatalf("malformed Social allowed signer: %q", definition.AllowedSigner)
	}
	directory := t.TempDir()
	publicKey := filepath.Join(directory, "social-v2.pub")
	if err := os.WriteFile(publicKey, []byte(strings.Join(fields[2:], " ")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fingerprint, err := exec.Command(sshKeygen, "-lf", publicKey, "-E", "sha256").CombinedOutput()
	if err != nil || !strings.Contains(string(fingerprint), "SHA256:L7rcTp4WYKPsYNmDx8ElbxwHlVc8VQvX9EH4SGlLcFQ") {
		t.Fatalf("Social v2 fingerprint=%q err=%v", fingerprint, err)
	}
	installFixture := func(version string) {
		t.Helper()
		for _, name := range []string{"SHA256SUMS", "SHA256SUMS.sig"} {
			encoded, err := os.ReadFile(filepath.Join("testdata", "social-"+version+"-"+name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, name), encoded, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	installFixture("v2")
	if err := verifySignature(context.Background(), systemCommandRunner{}, definition, directory); err != nil {
		t.Fatalf("v2 Social release signature rejected: %v", err)
	}
	installFixture("v1")
	if err := verifySignature(context.Background(), systemCommandRunner{}, definition, directory); err == nil {
		t.Fatal("retired v1 Social release signature accepted by v2 trust root")
	}
}

func TestDefaultCatalogResolvesCanonicalAndAliases(t *testing.T) {
	catalog := DefaultCatalog()
	for _, command := range []string{"social", "person", "company", "contact", "connections", "post", "evidence", "entity", "data", "providers"} {
		definition, ok := catalog.Resolve(command)
		if !ok || definition.Name != "social" {
			t.Fatalf("Resolve(%q) = %#v, %v", command, definition, ok)
		}
	}
	for _, command := range []string{"content", "media", "image", "video", "audio", "render", "remix"} {
		definition, ok := catalog.Resolve(command)
		if !ok || definition.Name != "content" {
			t.Fatalf("Resolve(%q) = %#v, %v", command, definition, ok)
		}
	}
	if _, ok := catalog.Resolve("auth"); ok {
		t.Fatal("reserved core command resolved as a plugin")
	}
	if _, ok := catalog.Resolve("unknown"); ok {
		t.Fatal("unknown command resolved as a plugin")
	}
}

func TestContentUsesIndependentRepositoryAndSigner(t *testing.T) {
	definition, ok := DefaultCatalog().Plugin("content")
	if !ok {
		t.Fatal("content plugin is missing")
	}
	if definition.Repository != "blazncloud/blazn-content" || definition.Executable != "blazn-content" {
		t.Fatalf("content distribution = %#v", definition)
	}
	if definition.SigningIdentity != "blazn-content-release" || definition.SignatureNamespace != "blazn-content-release" || definition.AllowedSigner == socialDefinition.AllowedSigner {
		t.Fatalf("content trust root = %#v", definition)
	}
}

func TestCatalogRejectsConflictingOwnership(t *testing.T) {
	definition := socialDefinition
	definition.CanonicalCommand = "auth"
	if _, err := NewCatalog([]Definition{definition}, []string{"auth"}); err == nil {
		t.Fatal("catalog accepted a reserved command conflict")
	}
	definition.CanonicalCommand = "social"
	definition.Aliases = []string{"person", "person"}
	if _, err := NewCatalog([]Definition{definition}, nil); err == nil {
		t.Fatal("catalog accepted duplicate aliases")
	}
}

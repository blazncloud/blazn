package plugin

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseViewArgsCanPinExactPluginVersion(t *testing.T) {
	latest, err := releaseViewArgs(socialDefinition, "")
	if err != nil || strings.Join(latest, " ") != "release view --repo blazncloud/blazn-social --json tagName" {
		t.Fatalf("latest args=%v err=%v", latest, err)
	}
	exact, err := releaseViewArgs(socialDefinition, "v0.1.0")
	if err != nil || strings.Join(exact, " ") != "release view v0.1.0 --repo blazncloud/blazn-social --json tagName" {
		t.Fatalf("exact args=%v err=%v", exact, err)
	}
	if _, err := releaseViewArgs(socialDefinition, "latest; unsafe"); err == nil {
		t.Fatal("unsafe plugin version override accepted")
	}
}

func TestCandidateHandshakeMustMatchSignedManifest(t *testing.T) {
	t.Setenv("GH_TOKEN", "installer-secret")
	t.Setenv("BLAZN_PLUGIN_VERSION", "v1.0.0")
	directory := t.TempDir()
	expected := validManifest("v1.0.0")
	encoded := `{"schemaVersion":1,"name":"social","version":"v1.0.0","protocolVersion":1,"minimumCoreVersion":"v1.0.0","executable":"blazn-social","commands":["social","person","company","contact","connections","saved-search","graph","post","evidence","entity","data","providers"]}`
	binary := filepath.Join(directory, "blazn-social")
	fixture := "#!/bin/sh\n[ -z \"${GH_TOKEN:-}\" ]\n[ -z \"${BLAZN_PLUGIN_VERSION:-}\" ]\nprintf '%s\\n' '" + encoded + "'\n"
	if err := os.WriteFile(binary, []byte(fixture), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := smokeTestCandidate(context.Background(), binary, expected); err != nil {
		t.Fatalf("matching candidate rejected: %v", err)
	}
	expected.Version = "v1.1.0"
	if err := smokeTestCandidate(context.Background(), binary, expected); err == nil {
		t.Fatal("candidate whose handshake differs from signed manifest was accepted")
	}
}

func TestSignedManifestVerificationRejectsTampering(t *testing.T) {
	sshKeygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen unavailable")
	}
	directory := t.TempDir()
	key := filepath.Join(directory, "signing-key")
	if output, err := exec.Command(sshKeygen, "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("generate key: %v: %s", err, output)
	}
	publicKey, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(directory, "SHA256SUMS")
	if err := os.WriteFile(manifestPath, []byte(strings.Repeat("0", 64)+"  plugin.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(sshKeygen, "-q", "-Y", "sign", "-f", key, "-n", "blazn-social-release", manifestPath).CombinedOutput(); err != nil {
		t.Fatalf("sign manifest: %v: %s", err, output)
	}
	definition := socialDefinition
	definition.SigningIdentity = "blazn-social-release"
	definition.SignatureNamespace = "blazn-social-release"
	definition.AllowedSigner = "blazn-social-release namespaces=\"blazn-social-release\" " + strings.TrimSpace(string(publicKey))
	if err := verifySignature(context.Background(), systemCommandRunner{}, definition, directory); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := os.WriteFile(manifestPath, []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifySignature(context.Background(), systemCommandRunner{}, definition, directory); err == nil {
		t.Fatal("tampered signed manifest accepted")
	}
}

func TestExtractSingleBinaryRejectsAdditionalMembers(t *testing.T) {
	directory := t.TempDir()
	archive := filepath.Join(directory, "plugin.tar.gz")
	writeArchive := func(names ...string) {
		file, err := os.Create(archive)
		if err != nil {
			t.Fatal(err)
		}
		gzipWriter := gzip.NewWriter(file)
		tarWriter := tar.NewWriter(gzipWriter)
		for _, name := range names {
			body := []byte("fixture plugin")
			if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o700, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
				t.Fatal(err)
			}
			if _, err := tarWriter.Write(body); err != nil {
				t.Fatal(err)
			}
		}
		if err := tarWriter.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gzipWriter.Close(); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}

	writeArchive("blazn-social")
	if err := extractSingleBinary(archive, "blazn-social", filepath.Join(directory, "good")); err != nil {
		t.Fatalf("valid archive rejected: %v", err)
	}
	writeArchive("blazn-social", "unexpected")
	if err := extractSingleBinary(archive, "blazn-social", filepath.Join(directory, "bad")); err == nil {
		t.Fatal("archive with additional member accepted")
	}
}

//go:build linux || darwin

package harnessworker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestProtectedExecutableVerifierAcceptsExactStableSnapshot(t *testing.T) {
	root, executable, digest := protectedExecutableFixture(t)
	verifier := ProtectedExecutableVerifier{TrustedRoot: root, RequiredOwnerUID: os.Geteuid()}
	if err := verifier.VerifyExecutable(context.Background(), executable, digest); err != nil {
		t.Fatalf("verify exact executable: %v", err)
	}
}

func TestProtectedExecutableVerifierRejectsUnsafeMetadataAndDigest(t *testing.T) {
	t.Run("digest mismatch", func(t *testing.T) {
		root, executable, _ := protectedExecutableFixture(t)
		if err := testExecutableVerifier(root).VerifyExecutable(context.Background(), executable, "sha256:"+fmt.Sprintf("%064x", 1)); ErrorCode(err) != "harness_executable_untrusted" {
			t.Fatalf("digest mismatch error=%v", err)
		}
	})
	t.Run("writable executable", func(t *testing.T) {
		root, executable, digest := protectedExecutableFixture(t)
		if err := os.Chmod(executable, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := testExecutableVerifier(root).VerifyExecutable(context.Background(), executable, digest); ErrorCode(err) != "harness_executable_untrusted" {
			t.Fatalf("writable executable error=%v", err)
		}
	})
	t.Run("hard link", func(t *testing.T) {
		root, executable, digest := protectedExecutableFixture(t)
		if err := os.Link(executable, filepath.Join(root, "hermes-copy")); err != nil {
			t.Fatal(err)
		}
		if err := testExecutableVerifier(root).VerifyExecutable(context.Background(), executable, digest); ErrorCode(err) != "harness_executable_untrusted" {
			t.Fatalf("hard-linked executable error=%v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		root, executable, digest := protectedExecutableFixture(t)
		link := filepath.Join(root, "hermes-link")
		if err := os.Symlink(executable, link); err != nil {
			t.Fatal(err)
		}
		if err := testExecutableVerifier(root).VerifyExecutable(context.Background(), link, digest); ErrorCode(err) != "harness_executable_untrusted" {
			t.Fatalf("symlink executable error=%v", err)
		}
	})
	t.Run("writable directory", func(t *testing.T) {
		root, executable, digest := protectedExecutableFixture(t)
		if err := os.Chmod(root, 0o775); err != nil {
			t.Fatal(err)
		}
		if err := testExecutableVerifier(root).VerifyExecutable(context.Background(), executable, digest); ErrorCode(err) != "harness_executable_untrusted" {
			t.Fatalf("writable directory error=%v", err)
		}
	})
	t.Run("writable parent permits trusted-root replacement", func(t *testing.T) {
		parent, root, executable, digest := protectedExecutableFixtureWithParent(t)
		if err := os.Rename(root, root+"-old"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(filepath.Join(root+"-old", "hermes"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(executable, body, 0o500); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(parent, 0o770); err != nil {
			t.Fatal(err)
		}
		if err := testExecutableVerifier(root).VerifyExecutable(context.Background(), executable, digest); ErrorCode(err) != "harness_executable_untrusted" {
			t.Fatalf("replacement below writable parent error=%v", err)
		}
	})
	t.Run("symlink trusted root", func(t *testing.T) {
		parent, root, executable, digest := protectedExecutableFixtureWithParent(t)
		realRoot := root + "-real"
		if err := os.Rename(root, realRoot); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realRoot, root); err != nil {
			t.Fatal(err)
		}
		executable = filepath.Join(root, filepath.Base(executable))
		if err := testExecutableVerifier(root).VerifyExecutable(context.Background(), executable, digest); ErrorCode(err) != "harness_executable_untrusted" {
			t.Fatalf("symlink trusted root error=%v parent=%s", err, parent)
		}
	})
	t.Run("wrong owner policy", func(t *testing.T) {
		root, executable, digest := protectedExecutableFixture(t)
		verifier := ProtectedExecutableVerifier{TrustedRoot: root, RequiredOwnerUID: os.Geteuid() + 1}
		if err := verifier.VerifyExecutable(context.Background(), executable, digest); ErrorCode(err) != "harness_executable_untrusted" {
			t.Fatalf("wrong owner error=%v", err)
		}
	})
	t.Run("oversized executable", func(t *testing.T) {
		root, executable, digest := protectedExecutableFixture(t)
		if err := os.Chmod(executable, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(executable, maxProtectedExecutableBytes+1); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(executable, 0o500); err != nil {
			t.Fatal(err)
		}
		if err := testExecutableVerifier(root).VerifyExecutable(context.Background(), executable, digest); ErrorCode(err) != "harness_executable_untrusted" {
			t.Fatalf("oversized executable error=%v", err)
		}
	})
}

func TestProtectedExecutableVerifierFailsClosedForPathAndCancellation(t *testing.T) {
	root, executable, digest := protectedExecutableFixture(t)
	verifier := testExecutableVerifier(root)
	outside := filepath.Join(filepath.Dir(root), "outside-hermes")
	if err := verifier.VerifyExecutable(context.Background(), outside, digest); ErrorCode(err) != "harness_executable_untrusted" {
		t.Fatalf("outside-root error=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := verifier.VerifyExecutable(ctx, executable, digest); ErrorCode(err) != "executable_verification_cancelled" {
		t.Fatalf("cancelled error=%v", err)
	}
}

func testExecutableVerifier(root string) ProtectedExecutableVerifier {
	return ProtectedExecutableVerifier{TrustedRoot: root, RequiredOwnerUID: os.Geteuid()}
}

func protectedExecutableFixture(t *testing.T) (string, string, string) {
	t.Helper()
	_, root, executable, digest := protectedExecutableFixtureWithParent(t)
	return root, executable, digest
}

func protectedExecutableFixtureWithParent(t *testing.T) (string, string, string, string) {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	parent, err := os.MkdirTemp(workingDirectory, ".protected-executable-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(parent); err != nil {
			t.Errorf("remove protected executable fixture: %v", err)
		}
	})
	root := filepath.Join(parent, "trusted")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte("reviewed hermes executable fixture\n")
	name := filepath.Join(root, "hermes")
	if err := os.WriteFile(name, body, 0o500); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	return parent, root, name, fmt.Sprintf("sha256:%x", digest)
}

//go:build darwin && blazn_qualification

package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDarwinQualificationKeychainRequiresExplicitSafePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.keychain-db")
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BLAZN_TEST_KEYCHAIN_PATH", path)
	if _, err := selectedDarwinKeychainPath(); err == nil {
		t.Fatal("test keychain path worked without explicit opt-in")
	}
	t.Setenv("BLAZN_ALLOW_TEST_KEYCHAIN", "1")
	got, err := selectedDarwinKeychainPath()
	if err != nil || got != path {
		t.Fatalf("path=%q err=%v", got, err)
	}
}

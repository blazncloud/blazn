//go:build darwin && !blazn_qualification

package auth

import "testing"

func TestProductionKeychainCannotBeRedirectedByEnvironment(t *testing.T) {
	t.Setenv("BLAZN_TEST_KEYCHAIN_PATH", "/tmp/attacker.keychain-db")
	t.Setenv("BLAZN_ALLOW_TEST_KEYCHAIN", "1")
	path, err := selectedDarwinKeychainPath()
	if err != nil || path != "" {
		t.Fatalf("production keychain path=%q err=%v", path, err)
	}
}

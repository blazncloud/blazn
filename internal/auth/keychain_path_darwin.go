//go:build darwin && !blazn_qualification

package auth

// Production builds always use the user's default Keychain. Environment
// variables cannot redirect credentials to another Keychain.
func selectedDarwinKeychainPath() (string, error) { return "", nil }

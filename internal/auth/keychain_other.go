//go:build !darwin

package auth

import "errors"

func storeDarwinCredential(string, []byte) error {
	return errors.New("macOS Keychain is unavailable on this platform")
}

func loadDarwinCredential(string) ([]byte, error) {
	return nil, errors.New("macOS Keychain is unavailable on this platform")
}

func deleteDarwinCredential(string) error {
	return errors.New("macOS Keychain is unavailable on this platform")
}

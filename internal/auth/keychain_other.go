//go:build !darwin

package auth

import "errors"

func storeDarwinCredential([]byte) error {
	return errors.New("macOS Keychain is unavailable on this platform")
}

func loadDarwinCredential() ([]byte, error) {
	return nil, errors.New("macOS Keychain is unavailable on this platform")
}

func deleteDarwinCredential() error {
	return errors.New("macOS Keychain is unavailable on this platform")
}

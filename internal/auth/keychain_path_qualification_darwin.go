//go:build darwin && blazn_qualification

package auth

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func selectedDarwinKeychainPath() (string, error) {
	path := os.Getenv("BLAZN_TEST_KEYCHAIN_PATH")
	if path == "" {
		return "", nil
	}
	if os.Getenv("BLAZN_ALLOW_TEST_KEYCHAIN") != "1" {
		return "", errors.New("BLAZN_TEST_KEYCHAIN_PATH requires BLAZN_ALLOW_TEST_KEYCHAIN=1")
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("test Keychain path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("test Keychain path must be an existing private regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		return "", errors.New("test Keychain path must be owned only by the current user")
	}
	return path, nil
}

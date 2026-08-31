//go:build linux || darwin

package harnessworker

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"syscall"
)

func openNoFollow(name string) (*os.File, error) {
	fd, err := syscall.Open(name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func protectedInfo(info os.FileInfo, maxBytes int64) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 && info.Mode().Perm() != 0o600 ||
		info.Size() < 1 || info.Size() > maxBytes {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return false
	}
	owner := int(stat.Uid)
	return owner == 0 || owner == os.Geteuid()
}

func digestBytes(body []byte) string {
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:])
}

//go:build linux || darwin

package harnessworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const maxProtectedExecutableBytes = 512 << 20

func verifyProtectedExecutable(ctx context.Context, trustedRoot, name, expectedDigest string, requiredOwnerUID int) error {
	if err := verifyDirectoryChain(trustedRoot, filepath.Dir(name), requiredOwnerUID); err != nil {
		return err
	}
	before, err := os.Lstat(name)
	if err != nil || !protectedExecutableInfo(before, requiredOwnerUID) {
		return errors.New("executable metadata is untrusted")
	}
	fd, err := syscall.Open(name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = syscall.Close(fd)
		return errors.New("open executable snapshot")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || !sameExecutableSnapshot(before, opened) || !protectedExecutableInfo(opened, requiredOwnerUID) {
		return errors.New("executable changed before hashing")
	}
	hash := sha256.New()
	buffer := make([]byte, 128<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			if _, err := hash.Write(buffer[:count]); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	after, err := file.Stat()
	pathAfter, pathErr := os.Lstat(name)
	if err != nil || pathErr != nil || !os.SameFile(opened, after) || !os.SameFile(after, pathAfter) ||
		!sameExecutableSnapshot(opened, after) || !sameExecutableSnapshot(after, pathAfter) || !protectedExecutableInfo(after, requiredOwnerUID) || !protectedExecutableInfo(pathAfter, requiredOwnerUID) {
		return errors.New("executable changed while hashing")
	}
	actualDigest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if actualDigest != expectedDigest {
		return errors.New("executable digest does not match")
	}
	return verifyDirectoryChain(trustedRoot, filepath.Dir(name), requiredOwnerUID)
}

func verifyDirectoryChain(trustedRoot, directory string, requiredOwnerUID int) error {
	current := directory
	atOrBelowTrustedRoot := true
	reachedTrustedRoot := false
	for {
		info, err := os.Lstat(current)
		if err != nil || !protectedDirectoryInfo(info, requiredOwnerUID, atOrBelowTrustedRoot) {
			return errors.New("executable directory is untrusted")
		}
		if current == trustedRoot {
			reachedTrustedRoot = true
			atOrBelowTrustedRoot = false
		}
		parent := filepath.Dir(current)
		if parent == current {
			if !reachedTrustedRoot {
				return errors.New("trusted executable root was not reached")
			}
			return nil
		}
		current = parent
	}
}

func protectedDirectoryInfo(info os.FileInfo, requiredOwnerUID int, requireExactOwner bool) bool {
	if info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	ownerUID := int(stat.Uid)
	return ownerUID == requiredOwnerUID || !requireExactOwner && ownerUID == 0
}

func protectedExecutableInfo(info os.FileInfo, requiredOwnerUID int) bool {
	if info == nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxProtectedExecutableBytes || info.Mode().Perm()&0o222 != 0 || info.Mode().Perm()&0o111 == 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1 && int(stat.Uid) == requiredOwnerUID
}

func sameExecutableSnapshot(left, right os.FileInfo) bool {
	return os.SameFile(left, right) && left.Size() == right.Size() && left.Mode() == right.Mode() && left.ModTime().Equal(right.ModTime()) && samePlatformSnapshot(left, right)
}

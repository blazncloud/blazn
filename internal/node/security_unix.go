//go:build !windows

package node

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func fileOwner(info os.FileInfo) (int64, uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int64(stat.Uid), uint64(stat.Nlink), true
}

func ensurePrivateDirectory(path string, uid int64) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("private directory path is not canonical")
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	for candidate := path; ; candidate = filepath.Dir(candidate) {
		info, err := os.Lstat(candidate)
		if err != nil {
			return err
		}
		owner, _, ok := fileOwner(info)
		writable := info.Mode().Perm()&0022 != 0
		stickyRoot := owner == 0 && info.Mode()&os.ModeSticky != 0
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (owner != uid && owner != 0) || (writable && !stickyRoot) {
			return fmt.Errorf("private directory boundary is unsafe: %s", candidate)
		}
		if candidate == string(filepath.Separator) {
			break
		}
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0700 {
		return errors.New("private directory must use mode 0700")
	}
	return nil
}

func lockInstallFile(path string) (func(), error) {
	var before os.FileInfo
	before, err := os.Lstat(path)
	if err == nil {
		owner, nlink, ok := fileOwner(before)
		if !ok || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || owner != currentUID() || nlink != 1 || before.Mode().Perm() != 0600 {
			return nil, errors.New("install lock path is unsafe")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	owner, nlink, ok := fileOwner(info)
	if !ok || owner != currentUID() || nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || (before != nil && !os.SameFile(before, info)) {
		file.Close()
		return nil, errors.New("install lock file is unsafe")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("another node install or recovery owns the lock")
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
}

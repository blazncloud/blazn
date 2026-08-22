//go:build !windows

package router

import (
	"errors"
	"os"
	"syscall"
)

func verifyPolicyOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() || stat.Nlink != 1 {
		return errors.New("policy is not owned by the current account")
	}
	return nil
}

func openPolicyFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

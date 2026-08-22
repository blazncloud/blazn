//go:build !windows

package router

import (
	"errors"
	"os"
	"syscall"
)

func verifyPolicyOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("policy is not owned by the current account")
	}
	return nil
}

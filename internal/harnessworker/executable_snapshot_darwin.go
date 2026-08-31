//go:build darwin

package harnessworker

import (
	"os"
	"syscall"
)

func samePlatformSnapshot(left, right os.FileInfo) bool {
	leftStat, leftOK := left.Sys().(*syscall.Stat_t)
	rightStat, rightOK := right.Sys().(*syscall.Stat_t)
	return leftOK && rightOK && leftStat.Dev == rightStat.Dev && leftStat.Ino == rightStat.Ino &&
		leftStat.Ctimespec.Sec == rightStat.Ctimespec.Sec && leftStat.Ctimespec.Nsec == rightStat.Ctimespec.Nsec
}

//go:build linux

package harnessworker

import (
	"os"
	"syscall"
)

func samePlatformSnapshot(left, right os.FileInfo) bool {
	leftStat, leftOK := left.Sys().(*syscall.Stat_t)
	rightStat, rightOK := right.Sys().(*syscall.Stat_t)
	return leftOK && rightOK && leftStat.Dev == rightStat.Dev && leftStat.Ino == rightStat.Ino &&
		leftStat.Ctim.Sec == rightStat.Ctim.Sec && leftStat.Ctim.Nsec == rightStat.Ctim.Nsec
}

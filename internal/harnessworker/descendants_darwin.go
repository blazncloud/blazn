//go:build darwin

package harnessworker

import (
	"syscall"
	"time"
)

type descendantTracker struct{}

func prepareDescendantTracking() (descendantTracker, error) { return descendantTracker{}, nil }

func (descendantTracker) restore() {}

func (descendantTracker) cleanup(processGroupID int, timeout time.Duration) (bool, bool) {
	_ = syscall.Kill(-processGroupID, syscall.SIGKILL)
	return waitProcessGroupGone(processGroupID, timeout), false
}

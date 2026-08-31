//go:build linux

package harnessworker

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

type descendantTracker struct {
	parent            int
	baseline          map[int]bool
	previousSubreaper int32
	locked            bool
}

var readProcessParents = processParents
var descendantTrackingMu sync.Mutex

func prepareDescendantTracking() (descendantTracker, error) {
	descendantTrackingMu.Lock()
	var previous int32
	if err := unix.Prctl(unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&previous)), 0, 0, 0); err != nil {
		descendantTrackingMu.Unlock()
		return descendantTracker{}, err
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		descendantTrackingMu.Unlock()
		return descendantTracker{}, err
	}
	parent := os.Getpid()
	parents, err := readProcessParents()
	if _, present := parents[parent]; err != nil || !present {
		_ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, uintptr(previous), 0, 0, 0)
		descendantTrackingMu.Unlock()
		return descendantTracker{}, errors.New("process inventory is unavailable")
	}
	return descendantTracker{parent: parent, baseline: descendantSet(parent, parents), previousSubreaper: previous, locked: true}, nil
}

func (tracker descendantTracker) restore() {
	_ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, uintptr(tracker.previousSubreaper), 0, 0, 0)
	if tracker.locked {
		descendantTrackingMu.Unlock()
	}
}

func (tracker descendantTracker) cleanup(processGroupID int, timeout time.Duration) (bool, bool) {
	deadline := time.Now().Add(timeout)
	quiet := 0
	for {
		_ = syscall.Kill(-processGroupID, syscall.SIGKILL)
		parents, err := readProcessParents()
		if _, present := parents[tracker.parent]; err != nil || !present {
			return false, false
		}
		targets := descendantSet(tracker.parent, parents)
		for pid := range tracker.baseline {
			delete(targets, pid)
		}
		if _, exists := parents[processGroupID]; exists {
			targets[processGroupID] = true
		}
		for pid := range targets {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			_, _ = syscall.Wait4(pid, nil, syscall.WNOHANG, nil)
		}
		groupGone := errors.Is(syscall.Kill(-processGroupID, 0), syscall.ESRCH)
		verified, err := readProcessParents()
		if _, present := verified[tracker.parent]; err != nil || !present {
			return false, false
		}
		remaining := descendantSet(tracker.parent, verified)
		for pid := range tracker.baseline {
			delete(remaining, pid)
		}
		if groupGone && len(remaining) == 0 {
			quiet++
			if quiet >= 2 {
				return true, true
			}
		} else {
			quiet = 0
		}
		if time.Now().After(deadline) {
			return groupGone, false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func processParents() (map[int]int, error) {
	parents := map[int]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid < 1 {
			continue
		}
		body, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		closing := strings.LastIndexByte(string(body), ')')
		if closing < 0 {
			return nil, errors.New("process inventory contains malformed stat")
		}
		fields := strings.Fields(string(body[closing+1:]))
		if len(fields) < 2 {
			return nil, errors.New("process inventory contains incomplete stat")
		}
		parent, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, errors.New("process inventory contains invalid parent")
		}
		parents[pid] = parent
	}
	if len(parents) == 0 {
		return nil, errors.New("process inventory is empty")
	}
	return parents, nil
}

func descendantSet(parent int, parents map[int]int) map[int]bool {
	result := map[int]bool{}
	changed := true
	for changed {
		changed = false
		for pid, processParent := range parents {
			if pid == parent || result[pid] {
				continue
			}
			if processParent == parent || result[processParent] {
				result[pid] = true
				changed = true
			}
		}
	}
	return result
}

//go:build linux

package harnessworker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestExecProcessRunnerTerminatesSetsidEscape(t *testing.T) {
	directory := t.TempDir()
	pidPath := filepath.Join(directory, "escaped.pid")
	spec, closeToken := helperProcessSpec(t, directory, "spawn-setsid-wait", pidPath)
	defer closeToken()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	result, err := (ExecProcessRunner{}).Run(ctx, spec)
	if err != nil || !result.TreeKilled || !result.CleanupComplete {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	body, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(body))
	if processExists(pid) {
		t.Fatalf("setsid descendant %d survived cleanup", pid)
	}
}

func TestDescendantTrackingFailsClosedWhenProcInventoryFails(t *testing.T) {
	original := readProcessParents
	readProcessParents = func() (map[int]int, error) { return nil, errors.New("unavailable") }
	t.Cleanup(func() { readProcessParents = original })
	if _, err := prepareDescendantTracking(); err == nil {
		t.Fatal("missing process inventory was accepted")
	}
}

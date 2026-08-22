//go:build linux

package main

import (
	"strings"
	"testing"
)

func TestIssuerStateUsesDistinctPrivilegedRoot(t *testing.T) {
	if stateRoot != "/var/lib/blazn-node-root/microk8s-worker-issuer" {
		t.Fatalf("issuer state root=%q", stateRoot)
	}
	if strings.HasPrefix(stateRoot, "/var/lib/blazn/") {
		t.Fatal("issuer durable state remains beneath the daemon-owned parent")
	}
}

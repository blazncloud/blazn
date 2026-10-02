package node

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/blazncloud/blazn/internal/client"
)

func TestUninstallRequarantinesAReleasedNode(t *testing.T) {
	released := `{"metadata":{"name":"worker-1","uid":"uid-1","resourceVersion":"41","labels":{"blazn.dev/node":"true","blazn.dev/sandbox-eligible":"true"}},"spec":{"taints":[{"key":"dedicated","value":"workers","effect":"NoSchedule"}]}}`
	quarantined := `{"metadata":{"name":"worker-1","uid":"uid-1","resourceVersion":"42","labels":{"blazn.dev/node":"true"}},"spec":{"taints":[{"key":"dedicated","value":"workers","effect":"NoSchedule"},{"key":"blazn.dev/bootstrap","value":"pending","effect":"NoSchedule"}]}}`
	var patch string
	patches := 0
	engine := NativeRootEngine{Platform: "linux", Commands: scriptedExecutor{run: func(path string, args []string, _ []byte) ([]byte, error) {
		switch args[0] {
		case "get":
			if patches == 0 {
				return []byte(released), nil
			}
			return []byte(quarantined), nil
		case "patch":
			patches++
			patch = args[5]
			return []byte(quarantined), nil
		}
		return nil, errors.New("unexpected kubectl operation")
	}}}
	join := &RootJoinBinding{ExpectedNodeName: "worker-1", ExpectedNodeUID: "uid-1", ExpectedResourceVersion: "41"}
	done, err := engine.requarantineReleasedNode(context.Background(), testJoinPlan("linux"), join)
	if err != nil || !done || patches != 1 || join.ExpectedResourceVersion != "42" {
		t.Fatalf("done=%v patches=%d rv=%s err=%v", done, patches, join.ExpectedResourceVersion, err)
	}
	var operations []map[string]any
	if json.Unmarshal([]byte(patch), &operations) != nil {
		t.Fatalf("patch is not JSON: %s", patch)
	}
	for _, required := range []string{`"op":"test","path":"/metadata/resourceVersion","value":"41"`, `"op":"remove","path":"/metadata/labels/blazn.dev~1sandbox-eligible"`, `{"key":"blazn.dev/bootstrap","value":"pending","effect":"NoSchedule"}`} {
		if !strings.Contains(patch, required) {
			t.Fatalf("quarantine patch lacks %s: %s", required, patch)
		}
	}
	done, err = engine.requarantineReleasedNode(context.Background(), testJoinPlan("linux"), join)
	if err != nil || done || patches != 1 {
		t.Fatalf("already quarantined node was patched again: done=%v patches=%d err=%v", done, patches, err)
	}
	foreign := NativeRootEngine{Platform: "linux", Commands: scriptedExecutor{run: func(path string, args []string, _ []byte) ([]byte, error) {
		return []byte(`{"metadata":{"name":"worker-1","uid":"uid-1","resourceVersion":"9","labels":{}},"spec":{"taints":[{"key":"blazn.dev/bootstrap","value":"foreign","effect":"NoSchedule"}]}}`), nil
	}}}
	if _, err := foreign.requarantineReleasedNode(context.Background(), testJoinPlan("linux"), join); err == nil {
		t.Fatal("foreign bootstrap taint variant was accepted")
	}
}

func TestUninstallQuarantineRetriesAConflictingPatch(t *testing.T) {
	previousAttempts, previousInterval := capacityReleaseAttempts, capacityReleaseInterval
	capacityReleaseAttempts, capacityReleaseInterval = 3, 0
	defer func() { capacityReleaseAttempts, capacityReleaseInterval = previousAttempts, previousInterval }()
	released := `{"metadata":{"name":"worker-1","uid":"uid-1","resourceVersion":"41","labels":{"blazn.dev/sandbox-eligible":"true"}},"spec":{}}`
	quarantined := `{"metadata":{"name":"worker-1","uid":"uid-1","resourceVersion":"43","labels":{}},"spec":{"taints":[{"key":"blazn.dev/bootstrap","value":"pending","effect":"NoSchedule"}]}}`
	patches := 0
	engine := NativeRootEngine{Platform: "linux", Commands: scriptedExecutor{run: func(path string, args []string, _ []byte) ([]byte, error) {
		if args[0] == "patch" {
			patches++
			if patches == 1 {
				return nil, errors.New("the object has been modified")
			}
			return []byte(quarantined), nil
		}
		if patches >= 2 {
			return []byte(quarantined), nil
		}
		return []byte(released), nil
	}}}
	taint := client.NodeInstallMutation{Ordinal: 10, Kind: "taint", Action: "apply", Target: "blazn.dev/bootstrap", Desired: map[string]any{"value": "pending", "effect": "NoSchedule"}}
	join := &RootJoinBinding{ExpectedNodeName: "worker-1", ExpectedNodeUID: "uid-1", ExpectedResourceVersion: "41"}
	if err := engine.rollback(context.Background(), testJoinPlan("linux"), taint, PriorState{State: "preexisting_exact"}, "/nonexistent", join); err != nil || patches != 2 {
		t.Fatalf("conflicting quarantine was not retried: patches=%d err=%v", patches, err)
	}
}

func TestFreshBootstrapRollbackLeavesAQuarantinedNodeQuarantined(t *testing.T) {
	// The control plane already returned the Node to quarantine (rebootstrap),
	// so rolling back the bootstrap taint must not remove it and make a still
	// joined worker schedulable before it leaves.
	quarantined := `{"metadata":{"name":"worker-1","uid":"uid-1","resourceVersion":"42","labels":{"blazn.dev/node":"true"}},"spec":{"taints":[{"key":"blazn.dev/bootstrap","value":"pending","effect":"NoSchedule"}]}}`
	patches := 0
	engine := NativeRootEngine{Platform: "linux", Commands: scriptedExecutor{run: func(path string, args []string, _ []byte) ([]byte, error) {
		switch args[0] {
		case "get":
			return []byte(quarantined), nil
		case "patch":
			patches++
			return []byte(quarantined), nil
		}
		return nil, errors.New("unexpected kubectl operation")
	}}}
	plan := testJoinPlan("linux")
	plan.Mode = client.NodeModeFresh
	mutation := client.NodeInstallMutation{Ordinal: 10, Kind: "taint", Action: "apply", Target: "blazn.dev/bootstrap",
		Desired: map[string]any{"key": "blazn.dev/bootstrap", "value": "pending", "effect": "NoSchedule"}}
	join := &RootJoinBinding{ExpectedNodeName: "worker-1", ExpectedNodeUID: "uid-1", ExpectedResourceVersion: "42"}
	if err := engine.rollback(context.Background(), plan, mutation, PriorState{State: "absent"}, t.TempDir(), join); err != nil || patches != 0 {
		t.Fatalf("err=%v patches=%d", err, patches)
	}
}

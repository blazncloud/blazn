package node

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/blazncloud/blazn/internal/client"
)

// A repair re-applies every signed mutation. On an activated node the
// control plane has already released capacity (bootstrap taint removed,
// eligibility label set); re-applying the bootstrap taint would leave the
// Node half released, which verification rejects, and under NodeRestriction
// the node's credential may not change taints at all.
func TestRepairDoesNotRequarantineAReleasedNode(t *testing.T) {
	released := `{"metadata":{"name":"worker-1","uid":"uid-1","resourceVersion":"41","labels":{"blazn.dev/node":"true","blazn.dev/sandbox-eligible":"true"}},"spec":{"taints":[{"key":"blazn.dev/sandbox-node","value":"true","effect":"NoSchedule"}]}}`
	patches := []string{}
	engine := NativeRootEngine{Platform: "linux", Commands: scriptedExecutor{run: func(path string, args []string, _ []byte) ([]byte, error) {
		switch args[0] {
		case "get":
			return []byte(released), nil
		case "patch":
			patches = append(patches, strings.Join(args, " "))
			return []byte(released), nil
		}
		return nil, errors.New("unexpected kubectl operation")
	}}}
	plan := testJoinPlan("linux")
	plan.Mode = client.NodeModeFresh
	bootstrap := client.NodeInstallMutation{Ordinal: 10, Kind: "taint", Action: "apply", Target: "blazn.dev/bootstrap",
		Desired: map[string]any{"key": "blazn.dev/bootstrap", "value": "pending", "effect": "NoSchedule"}}
	plan.Mutations = append(plan.Mutations, bootstrap)
	join := &RootJoinBinding{ClusterID: plan.Cluster.ID, ExpectedNodeName: "worker-1", ExpectedNodeUID: "uid-1", ExpectedResourceVersion: "41"}
	if err := engine.apply(context.Background(), plan, bootstrap, nil, join); err != nil {
		t.Fatal(err)
	}
	if len(patches) != 0 {
		t.Fatalf("a released Node was re-quarantined: %v", patches)
	}
	if _, err := engine.verify(context.Background(), plan, join); err != nil {
		t.Fatalf("verify after repair: %v", err)
	}
}

func TestInstallStillAppliesTheBootstrapTaintToAQuarantinedNode(t *testing.T) {
	// During a first install the Node is still in bootstrap quarantine and
	// the mutation is applied exactly as before.
	quarantined := `{"metadata":{"name":"worker-1","uid":"uid-1","resourceVersion":"41","labels":{"blazn.dev/node":"true"}},"spec":{"taints":[{"key":"blazn.dev/sandbox-node","value":"true","effect":"NoSchedule"},{"key":"blazn.dev/bootstrap","value":"pending","effect":"NoSchedule"}]}}`
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
	bootstrap := client.NodeInstallMutation{Ordinal: 10, Kind: "taint", Action: "apply", Target: "blazn.dev/bootstrap",
		Desired: map[string]any{"key": "blazn.dev/bootstrap", "value": "pending", "effect": "NoSchedule"}}
	join := &RootJoinBinding{ClusterID: plan.Cluster.ID, ExpectedNodeName: "worker-1", ExpectedNodeUID: "uid-1", ExpectedResourceVersion: "41"}
	if err := engine.apply(context.Background(), plan, bootstrap, nil, join); err != nil || patches != 1 {
		t.Fatalf("err=%v patches=%d", err, patches)
	}
}

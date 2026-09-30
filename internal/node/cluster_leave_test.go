package node

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/client"
)

func TestLeaveClusterRetiresTheNodeWaitsForEvictionThenLeaves(t *testing.T) {
	previousAttempts, previousInterval := leaveEvictionAttempts, leaveEvictionInterval
	leaveEvictionAttempts, leaveEvictionInterval = 5, time.Millisecond
	defer func() { leaveEvictionAttempts, leaveEvictionInterval = previousAttempts, previousInterval }()
	var steps []string
	var patch []map[string]any
	podLists := []string{
		`{"items":[{"spec":{"tolerations":[{"operator":"Exists","effect":"NoSchedule"}]}},{"spec":{"tolerations":[{"operator":"Exists"}]}}]}`,
		`{"items":[{"spec":{"tolerations":[{"operator":"Exists"}]}}]}`,
	}
	engine := NativeRootEngine{Platform: "linux", allowTestJoinRuntime: true, Commands: scriptedExecutor{run: func(path string, args []string, _ []byte) ([]byte, error) {
		if path == microK8sCommandPath {
			steps = append(steps, "leave:"+strings.Join(args, " "))
			return nil, nil
		}
		if path != "/snap/bin/microk8s.kubectl" || len(args) == 0 {
			t.Fatalf("unexpected command %s %v", path, args)
		}
		switch args[0] {
		case "get":
			if args[1] == "node" {
				steps = append(steps, "get-node")
				return []byte(`{"metadata":{"name":"worker-1","uid":"uid-1","resourceVersion":"7"},"spec":{"taints":[{"key":"blazn.dev/sandbox-node","value":"true","effect":"NoSchedule"}]}}`), nil
			}
			steps = append(steps, "get-pods:"+args[len(args)-3])
			next := podLists[0]
			if len(podLists) > 1 {
				podLists = podLists[1:]
			}
			return []byte(next), nil
		case "patch":
			steps = append(steps, "patch")
			if err := json.Unmarshal([]byte(args[5]), &patch); err != nil {
				t.Fatal(err)
			}
			return []byte(`{"metadata":{"name":"worker-1","uid":"uid-1","resourceVersion":"8"},"spec":{"unschedulable":true}}`), nil
		}
		t.Fatalf("unexpected kubectl %v", args)
		return nil, nil
	}}}
	join := &RootJoinBinding{ClusterID: "cluster-1", ExpectedNodeName: "worker-1", ExpectedNodeUID: "uid-1"}
	if err := engine.leaveCluster(context.Background(), client.NodeInstallPlan{Mode: client.NodeModeFresh}, join); err != nil {
		t.Fatal(err)
	}
	want := []string{"get-node", "patch", "get-pods:spec.nodeName=worker-1", "get-pods:spec.nodeName=worker-1", "leave:leave"}
	if strings.Join(steps, ",") != strings.Join(want, ",") {
		t.Fatalf("steps=%v", steps)
	}
	encoded, _ := json.Marshal(patch)
	for _, fragment := range []string{`"path":"/metadata/uid","value":"uid-1"`, `"path":"/spec/unschedulable","value":true`, `"key":"blazn.dev/sandbox-node"`, `{"effect":"NoExecute","key":"blazn.dev/retired","value":"true"}`} {
		if !strings.Contains(string(encoded), fragment) {
			t.Fatalf("patch %s lacks %s", encoded, fragment)
		}
	}
	if err := engine.leaveCluster(context.Background(), client.NodeInstallPlan{}, &RootJoinBinding{ExpectedNodeName: "worker-1"}); err == nil {
		t.Fatal("leave without a bound Node UID must fail")
	}
}

func TestPodsPendingRetirementRequiresANoExecuteToleration(t *testing.T) {
	for input, pending := range map[string]bool{
		`{"items":[]}`: false,
		`{"items":[{"spec":{"tolerations":[{"operator":"Exists"}]}}]}`:                                                 false,
		`{"items":[{"spec":{"tolerations":[{"key":"blazn.dev/retired","operator":"Exists"}]}}]}`:                       false,
		`{"items":[{"spec":{"tolerations":[{"operator":"Exists","effect":"NoSchedule"}]}}]}`:                           true,
		`{"items":[{"spec":{"tolerations":[{"key":"blazn.dev/sandbox-node","value":"true","effect":"NoSchedule"}]}}]}`: true,
		`{"items":[{"spec":{}}]}`: true,
		`not json`:                true,
	} {
		if got := podsPendingRetirement([]byte(input)); got != pending {
			t.Fatalf("%s: pending=%v", input, got)
		}
	}
}

func TestJoinedNodeIsExcludedFromExternalLoadBalancers(t *testing.T) {
	labelled := false
	var patch string
	engine := NativeRootEngine{Platform: "linux", allowTestJoinRuntime: true, Commands: scriptedExecutor{run: func(path string, args []string, _ []byte) ([]byte, error) {
		switch args[0] {
		case "get":
			if labelled {
				return []byte(`{"metadata":{"name":"worker-1","uid":"uid-1","resourceVersion":"9","labels":{"blazn.dev/node":"true","node.kubernetes.io/exclude-from-external-load-balancers":"true"}}}`), nil
			}
			return []byte(`{"metadata":{"name":"worker-1","uid":"uid-1","resourceVersion":"8","labels":{"blazn.dev/node":"true"}}}`), nil
		case "patch":
			patch, labelled = args[5], true
			return []byte(`{}`), nil
		}
		t.Fatalf("unexpected %s %v", path, args)
		return nil, nil
	}}}
	refreshed, err := engine.excludeFromExternalLoadBalancers(context.Background(), client.NodeInstallPlan{}, JoinedNode{Name: "worker-1", UID: "uid-1", ResourceVersion: "7"})
	if err != nil || refreshed.ResourceVersion != "9" || refreshed.UID != "uid-1" {
		t.Fatalf("refreshed=%#v err=%v", refreshed, err)
	}
	if !strings.Contains(patch, `"path":"/metadata/labels/node.kubernetes.io~1exclude-from-external-load-balancers","value":"true"`) || !strings.Contains(patch, `"path":"/metadata/uid","value":"uid-1"`) {
		t.Fatalf("patch=%s", patch)
	}
	if _, err := engine.excludeFromExternalLoadBalancers(context.Background(), client.NodeInstallPlan{}, JoinedNode{Name: "worker-1", UID: "other"}); err == nil {
		t.Fatal("a changed Node UID must fail")
	}
}

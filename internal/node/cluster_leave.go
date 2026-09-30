package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/client"
)

// A retiring worker must not stay joined to the shared cluster. Before it
// leaves, it marks its own Node unschedulable and adds a NoExecute retirement
// taint so the cluster evicts every Pod that does not tolerate it (sandbox,
// ingress, load-balancer and log-shipper Pods alike) and drops their Service
// endpoints while the node is still reachable. The kubelet credential can
// patch its own Node but cannot delete it; the control plane deletes the Node
// object when the node is retired.
const (
	RetiredNodeTaintKey   = "blazn.dev/retired"
	microK8sClusteredLock = "/var/snap/microk8s/current/var/lock/clustered.lock"
	microK8sCommandPath   = "/snap/bin/microk8s"
)

var leaveEvictionAttempts, leaveEvictionInterval = 45, 2 * time.Second

func (e NativeRootEngine) leaveCluster(ctx context.Context, plan client.NodeInstallPlan, join *RootJoinBinding) error {
	if join == nil || join.ExpectedNodeName == "" || join.ExpectedNodeUID == "" {
		return errors.New("cluster leave requires the root-authorized Node binding")
	}
	joined, err := e.workerJoined(ctx, plan)
	if err != nil || !joined {
		return err
	}
	for attempt := 1; ; attempt++ {
		err := e.markNodeRetired(ctx, plan, join)
		if !errors.Is(err, errCapacityReleaseConflict) || attempt >= capacityReleaseAttempts {
			if err != nil {
				return err
			}
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(capacityReleaseInterval):
		}
	}
	if err := e.waitForRetirementEviction(ctx, plan, join.ExpectedNodeName); err != nil {
		return err
	}
	_, err = e.microK8s(ctx, plan, "leave")
	return err
}

// workerJoined reports whether MicroK8s still holds cluster membership. Join
// creates var/lock/clustered.lock and leave removes it, so a retry after a
// completed leave is a no-op instead of acting on the standalone runtime.
func (e NativeRootEngine) workerJoined(ctx context.Context, plan client.NodeInstallPlan) (bool, error) {
	if e.Platform == "linux" {
		if e.allowTestJoinRuntime {
			return true, nil
		}
		if _, err := os.Stat(microK8sClusteredLock); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	vm, err := readLimaVM(plan, e.LimaBindingPath)
	if err != nil {
		return false, err
	}
	_, err = e.Commands.Run(ctx, "/usr/local/bin/limactl", "shell", vm, "test", "-e", microK8sClusteredLock)
	var exit *FixedCommandError
	if errors.As(err, &exit) && exit.ExitCode == 1 {
		return false, nil
	}
	return err == nil, err
}

func (e NativeRootEngine) microK8s(ctx context.Context, plan client.NodeInstallPlan, args ...string) ([]byte, error) {
	if e.Platform == "linux" {
		return e.Commands.Run(ctx, microK8sCommandPath, args...)
	}
	vm, err := readLimaVM(plan, e.LimaBindingPath)
	if err != nil {
		return nil, err
	}
	return e.Commands.Run(ctx, "/usr/local/bin/limactl", append([]string{"shell", vm, "sudo", microK8sCommandPath}, args...)...)
}

func (e NativeRootEngine) markNodeRetired(ctx context.Context, plan client.NodeInstallPlan, join *RootJoinBinding) error {
	state, err := e.readCapacityNode(ctx, plan, join.ExpectedNodeName)
	if err != nil {
		return err
	}
	if state.UID != join.ExpectedNodeUID {
		return errors.New("retiring node UID differs from binding")
	}
	retired := false
	for _, taint := range state.Taints {
		if taint.Key == RetiredNodeTaintKey && taint.Effect == "NoExecute" {
			retired = true
		}
	}
	if retired && state.Unschedulable != nil && *state.Unschedulable {
		return nil
	}
	operations := []map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": state.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": state.ResourceVersion},
		{"op": "add", "path": "/spec/unschedulable", "value": true},
	}
	if !retired {
		taints := append(append([]clusterTaint(nil), state.Taints...), clusterTaint{Key: RetiredNodeTaintKey, Value: "true", Effect: "NoExecute"})
		if state.TaintsPresent {
			operations = append(operations, map[string]any{"op": "test", "path": "/spec/taints", "value": state.Taints}, map[string]any{"op": "replace", "path": "/spec/taints", "value": taints})
		} else {
			operations = append(operations, map[string]any{"op": "add", "path": "/spec/taints", "value": taints})
		}
	}
	encoded, err := json.Marshal(operations)
	if err != nil {
		return err
	}
	output, err := e.kubectl(ctx, plan, "patch", "node", join.ExpectedNodeName, "--type=json", "--patch", string(encoded), "-o", "json")
	if err != nil {
		return errCapacityReleaseConflict
	}
	patched, err := decodeCapacityNode(output)
	if err != nil || patched.UID != join.ExpectedNodeUID {
		return errors.New("retirement response differs from binding")
	}
	return nil
}

// waitForRetirementEviction waits, bounded, until every Pod still bound to
// the node tolerates the retirement taint (the network plugin typically
// does). A Pod that outlives the bound is left to the node lifecycle
// controller; leaving must not be blocked by a slow termination.
func (e NativeRootEngine) waitForRetirementEviction(ctx context.Context, plan client.NodeInstallPlan, name string) error {
	for attempt := 1; attempt <= leaveEvictionAttempts; attempt++ {
		output, err := e.kubectl(ctx, plan, "get", "pods", "--all-namespaces", "--field-selector", "spec.nodeName="+name, "-o", "json")
		if err == nil && !podsPendingRetirement(output) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(leaveEvictionInterval):
		}
	}
	return nil
}

func podsPendingRetirement(output []byte) bool {
	var list struct {
		Items []struct {
			Spec struct {
				Tolerations []struct{ Key, Operator, Value, Effect string } `json:"tolerations"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal(output, &list) != nil {
		return true
	}
	for _, pod := range list.Items {
		tolerated := false
		for _, toleration := range pod.Spec.Tolerations {
			if toleration.Effect != "" && toleration.Effect != "NoExecute" {
				continue
			}
			if (toleration.Operator == "Exists" && (toleration.Key == "" || toleration.Key == RetiredNodeTaintKey)) ||
				(toleration.Key == RetiredNodeTaintKey && (toleration.Operator == "" || toleration.Operator == "Equal") && toleration.Value == "true") {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return true
		}
	}
	return false
}

// excludeFromExternalLoadBalancers labels a freshly joined worker so that
// load-balancer speakers never announce service addresses from it, then
// returns the refreshed Node. The permanent sandbox-node taint already keeps
// such speakers off; the label also covers a speaker that tolerates it.
func (e NativeRootEngine) excludeFromExternalLoadBalancers(ctx context.Context, plan client.NodeInstallPlan, joined JoinedNode) (JoinedNode, error) {
	for attempt := 1; ; attempt++ {
		state, err := e.readCapacityNode(ctx, plan, joined.Name)
		if err != nil {
			return JoinedNode{}, err
		}
		if state.UID != joined.UID {
			return JoinedNode{}, errors.New("joined node UID changed before load-balancer exclusion")
		}
		if state.Labels[externalLoadBalancerExclusion] == "true" {
			return JoinedNode{Name: state.Name, UID: state.UID, ResourceVersion: state.ResourceVersion}, nil
		}
		path := "/metadata/labels/" + strings.ReplaceAll(strings.ReplaceAll(externalLoadBalancerExclusion, "~", "~0"), "/", "~1")
		operations := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": state.UID}, {"op": "test", "path": "/metadata/resourceVersion", "value": state.ResourceVersion}}
		if state.LabelsPresent {
			operations = append(operations, map[string]any{"op": "add", "path": path, "value": "true"})
		} else {
			operations = append(operations, map[string]any{"op": "add", "path": "/metadata/labels", "value": map[string]string{externalLoadBalancerExclusion: "true"}})
		}
		encoded, err := json.Marshal(operations)
		if err != nil {
			return JoinedNode{}, err
		}
		if _, err := e.kubectl(ctx, plan, "patch", "node", joined.Name, "--type=json", "--patch", string(encoded), "-o", "json"); err != nil && attempt >= capacityReleaseAttempts {
			return JoinedNode{}, errors.New("joined node could not be excluded from external load balancers")
		} else if err != nil {
			select {
			case <-ctx.Done():
				return JoinedNode{}, ctx.Err()
			case <-time.After(capacityReleaseInterval):
			}
		}
	}
}

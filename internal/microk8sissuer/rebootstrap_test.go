package microk8sissuer

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
)

func TestBackendRebootstrapReturnsAReleasedWorkerToQuarantine(t *testing.T) {
	node := func(labels, taints string) []byte {
		return []byte(`{"metadata":{"name":"worker-1","uid":"` + retireUID + `","resourceVersion":"41","labels":{` + labels + `}},"spec":{"taints":[` + taints + `]}}`)
	}
	sandboxTaint := `{"key":"blazn.dev/sandbox-node","value":"true","effect":"NoSchedule"}`
	bootstrap := `,{"key":"blazn.dev/bootstrap","value":"pending","effect":"NoSchedule"}`
	hold := `,{"key":"` + PlacementHoldTaint + `","value":"paused","effect":"NoSchedule"}`
	released := `"blazn.dev/node":"true","` + WorkspaceNodeLabel + `":"11111111-2222-4333-8444-555555555555","` + SandboxEligibleLabel + `":"true"`
	for _, tc := range []struct {
		name, labels, taints, uid  string
		rebootstrapped             bool
		wantTaints, wantLabelsKeys string
		code                       string
	}{
		{name: "released worker", labels: released, taints: sandboxTaint + hold, uid: retireUID, rebootstrapped: true,
			wantTaints: "blazn.dev/sandbox-node," + PlacementHoldTaint + "," + BootstrapTaintKey, wantLabelsKeys: "blazn.dev/node," + WorkspaceNodeLabel},
		{name: "already quarantined", labels: `"blazn.dev/node":"true"`, taints: sandboxTaint + bootstrap, uid: retireUID},
		{name: "taint present but still eligible", labels: released, taints: sandboxTaint + bootstrap, uid: retireUID, rebootstrapped: true,
			wantTaints: "blazn.dev/sandbox-node," + BootstrapTaintKey, wantLabelsKeys: "blazn.dev/node," + WorkspaceNodeLabel},
		{name: "foreign bootstrap variant", labels: `"blazn.dev/node":"true"`, taints: sandboxTaint + `,{"key":"blazn.dev/bootstrap","value":"other","effect":"NoSchedule"}`, uid: retireUID, code: "rebootstrap_rejected"},
		{name: "not a Blazn node", labels: `"kubernetes.io/hostname":"worker-1"`, uid: retireUID, code: "rebootstrap_rejected"},
		{name: "MicroK8s control plane", labels: `"blazn.dev/node":"true","node.kubernetes.io/microk8s-controlplane":"microk8s-controlplane"`, taints: sandboxTaint, uid: retireUID, code: "rebootstrap_rejected"},
		{name: "different UID", labels: released, taints: sandboxTaint, uid: "7f3c2a10-0000-4000-8000-000000000002", code: "binding_conflict"},
	} {
		runner := &scriptedRunner{outputs: map[string][]byte{"get": node(tc.labels, tc.taints), "patch": nil}}
		changed, err := testBackend(runner).Rebootstrap(context.Background(), "worker-1", tc.uid)
		var protocol *ProtocolError
		if tc.code != "" {
			if !errors.As(err, &protocol) || protocol.Code != tc.code || changed || len(runner.calls) != 1 {
				t.Fatalf("%s: changed=%v err=%v calls=%v", tc.name, changed, err, runner.calls)
			}
			continue
		}
		if err != nil || changed != tc.rebootstrapped {
			t.Fatalf("%s: changed=%v err=%v", tc.name, changed, err)
		}
		if !tc.rebootstrapped {
			if len(runner.calls) != 1 {
				t.Fatalf("%s: a quarantined Node was patched: %v", tc.name, runner.calls)
			}
			continue
		}
		prefix := "patch node worker-1 --type=json -p "
		if len(runner.calls) != 2 || !strings.HasPrefix(runner.calls[1], prefix) {
			t.Fatalf("%s: calls=%v", tc.name, runner.calls)
		}
		var patch []struct {
			Op, Path string
			Value    json.RawMessage
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(runner.calls[1], prefix)), &patch); err != nil || len(patch) != 3 ||
			patch[0].Op != "test" || patch[0].Path != "/metadata/resourceVersion" || string(patch[0].Value) != `"41"` ||
			patch[1].Path != "/metadata/labels" || patch[2].Path != "/spec/taints" {
			t.Fatalf("%s: patch=%s", tc.name, runner.calls[1])
		}
		var labels map[string]string
		var taints []struct{ Key, Value, Effect string }
		if json.Unmarshal(patch[1].Value, &labels) != nil || json.Unmarshal(patch[2].Value, &taints) != nil {
			t.Fatalf("%s: undecodable patch", tc.name)
		}
		keys := []string{}
		for key := range labels {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != tc.wantLabelsKeys {
			t.Fatalf("%s: labels=%v want %s", tc.name, keys, tc.wantLabelsKeys)
		}
		taintKeys := []string{}
		for _, taint := range taints {
			taintKeys = append(taintKeys, taint.Key)
			if taint.Key == BootstrapTaintKey && (taint.Value != "pending" || taint.Effect != "NoSchedule") {
				t.Fatalf("%s: bootstrap taint %+v", tc.name, taint)
			}
		}
		if strings.Join(taintKeys, ",") != tc.wantTaints {
			t.Fatalf("%s: taints=%v want %s", tc.name, taintKeys, tc.wantTaints)
		}
	}
}

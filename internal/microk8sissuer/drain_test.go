package microk8sissuer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestBackendDrainMarksOnlyABlaznWorkerAsLeaving(t *testing.T) {
	node := func(labels, taints string, unschedulable bool) []byte {
		cordon := ""
		if unschedulable {
			cordon = `"unschedulable":true,`
		}
		return []byte(`{"metadata":{"name":"worker-1","uid":"` + retireUID + `","resourceVersion":"41","labels":{` + labels + `}},"spec":{` + cordon + `"taints":[` + taints + `]}}`)
	}
	sandboxTaint := `{"key":"blazn.dev/sandbox-node","value":"true","effect":"NoSchedule"}`
	hold := `,{"key":"` + PlacementHoldTaint + `","value":"paused","effect":"NoSchedule"}`
	retired := `,{"key":"` + RetiredTaintKey + `","value":"true","effect":"NoExecute"}`
	for _, tc := range []struct {
		name, labels, taints, uid string
		unschedulable, drained    bool
		wantTaints                string
		code                      string
	}{
		{name: "active worker", labels: `"blazn.dev/node":"true"`, taints: sandboxTaint + hold, uid: retireUID, drained: true, wantTaints: "blazn.dev/sandbox-node," + PlacementHoldTaint + "," + RetiredTaintKey},
		{name: "already drained", taints: sandboxTaint + retired, unschedulable: true, uid: retireUID},
		{name: "tainted but schedulable", taints: sandboxTaint + retired, uid: retireUID, drained: true, wantTaints: "blazn.dev/sandbox-node," + RetiredTaintKey},
		{name: "not a Blazn node", labels: `"kubernetes.io/hostname":"worker-1"`, uid: retireUID, code: "drain_rejected"},
		{name: "MicroK8s control plane", labels: `"blazn.dev/node":"true","node.kubernetes.io/microk8s-controlplane":"microk8s-controlplane"`, taints: sandboxTaint, uid: retireUID, code: "drain_rejected"},
		{name: "different UID", taints: sandboxTaint, uid: "7f3c2a10-0000-4000-8000-000000000002", code: "binding_conflict"},
	} {
		runner := &scriptedRunner{outputs: map[string][]byte{"get": node(tc.labels, tc.taints, tc.unschedulable), "patch": nil}}
		drained, err := testBackend(runner).Drain(context.Background(), "worker-1", tc.uid)
		var protocol *ProtocolError
		if tc.code != "" {
			if !errors.As(err, &protocol) || protocol.Code != tc.code || drained || len(runner.calls) != 1 {
				t.Fatalf("%s: drained=%v err=%v calls=%v", tc.name, drained, err, runner.calls)
			}
			continue
		}
		if err != nil || drained != tc.drained {
			t.Fatalf("%s: drained=%v err=%v", tc.name, drained, err)
		}
		if !tc.drained {
			if len(runner.calls) != 1 {
				t.Fatalf("%s: a drained Node was patched: %v", tc.name, runner.calls)
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
			patch[1].Path != "/spec/taints" || patch[2].Path != "/spec/unschedulable" || string(patch[2].Value) != "true" {
			t.Fatalf("%s: patch=%s", tc.name, runner.calls[1])
		}
		var taints []struct{ Key, Value, Effect string }
		if err := json.Unmarshal(patch[1].Value, &taints); err != nil {
			t.Fatal(err)
		}
		keys := []string{}
		for _, taint := range taints {
			keys = append(keys, taint.Key)
			if taint.Key == RetiredTaintKey && (taint.Value != "true" || taint.Effect != "NoExecute") {
				t.Fatalf("%s: retired taint %+v", tc.name, taint)
			}
		}
		if strings.Join(keys, ",") != tc.wantTaints {
			t.Fatalf("%s: taints=%v want %s", tc.name, keys, tc.wantTaints)
		}
	}
}

func TestDecodeDrainRequestIsClosed(t *testing.T) {
	base := `{"schemaVersion":"` + SchemaVersion + `","operation":"drain","clusterId":"cluster-a","expectedNodeName":"worker-1","nodeUid":"` + retireUID + `"`
	if _, err := DecodeRequest([]byte(base + `}`)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{base + `,"holdReason":"paused"}`, `{"schemaVersion":"` + SchemaVersion + `","operation":"drain","clusterId":"cluster-a","nodeUid":"` + retireUID + `"}`} {
		if _, err := DecodeRequest([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

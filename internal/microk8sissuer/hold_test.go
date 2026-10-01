package microk8sissuer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestBackendHoldSetsChangesAndClearsOnlyThePlacementHoldTaint(t *testing.T) {
	node := func(labels, taints string) []byte {
		return []byte(`{"metadata":{"name":"worker-1","uid":"` + retireUID + `","resourceVersion":"41","labels":{` + labels + `}},"spec":{"taints":[` + taints + `]}}`)
	}
	sandboxTaint := `{"key":"blazn.dev/sandbox-node","value":"true","effect":"NoSchedule"}`
	held := func(reason string) string {
		return `,{"key":"` + PlacementHoldTaint + `","value":"` + reason + `","effect":"NoSchedule"}`
	}
	for _, tc := range []struct {
		name, labels, taints, uid, reason string
		changed                           bool
		want                              []string
		code                              string
	}{
		{name: "hold an active worker", labels: `"blazn.dev/node":"true"`, taints: sandboxTaint, uid: retireUID, reason: "quarantined", changed: true, want: []string{"blazn.dev/sandbox-node=true", PlacementHoldTaint + "=quarantined"}},
		{name: "already held for the reason", taints: sandboxTaint + held("paused"), uid: retireUID, reason: "paused"},
		{name: "change the reason", taints: sandboxTaint + held("offline"), uid: retireUID, reason: "paused", changed: true, want: []string{"blazn.dev/sandbox-node=true", PlacementHoldTaint + "=paused"}},
		{name: "release", taints: sandboxTaint + held("paused"), uid: retireUID, changed: true, want: []string{"blazn.dev/sandbox-node=true"}},
		{name: "release when not held", taints: sandboxTaint, uid: retireUID},
		{name: "not a Blazn node", labels: `"kubernetes.io/hostname":"worker-1"`, uid: retireUID, reason: "paused", code: "hold_rejected"},
		{name: "control plane", labels: `"node-role.kubernetes.io/control-plane":""`, taints: sandboxTaint, uid: retireUID, reason: "paused", code: "hold_rejected"},
		{name: "MicroK8s control plane", labels: `"blazn.dev/node":"true","node.kubernetes.io/microk8s-controlplane":"microk8s-controlplane"`, taints: sandboxTaint, uid: retireUID, reason: "paused", code: "hold_rejected"},
		{name: "different UID", taints: sandboxTaint, uid: "7f3c2a10-0000-4000-8000-000000000002", reason: "paused", code: "binding_conflict"},
	} {
		runner := &scriptedRunner{outputs: map[string][]byte{"get": node(tc.labels, tc.taints), "patch": nil}}
		changed, err := testBackend(runner).Hold(context.Background(), "worker-1", tc.uid, tc.reason)
		var protocol *ProtocolError
		if tc.code != "" {
			if !errors.As(err, &protocol) || protocol.Code != tc.code || len(runner.calls) != 1 {
				t.Fatalf("%s: err=%v calls=%v", tc.name, err, runner.calls)
			}
			continue
		}
		if err != nil || changed != tc.changed {
			t.Fatalf("%s: changed=%v err=%v", tc.name, changed, err)
		}
		if !tc.changed {
			if len(runner.calls) != 1 {
				t.Fatalf("%s: unchanged Node was patched: %v", tc.name, runner.calls)
			}
			continue
		}
		call := runner.calls[1]
		prefix := "patch node worker-1 --type=json -p "
		if !strings.HasPrefix(call, prefix) {
			t.Fatalf("%s: patch call %q", tc.name, call)
		}
		var patch []struct {
			Op, Path string
			Value    json.RawMessage
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(call, prefix)), &patch); err != nil || len(patch) != 2 ||
			patch[0].Op != "test" || patch[0].Path != "/metadata/resourceVersion" || string(patch[0].Value) != `"41"` ||
			patch[1].Op != "add" || patch[1].Path != "/spec/taints" {
			t.Fatalf("%s: patch=%s", tc.name, call)
		}
		var taints []struct{ Key, Value, Effect string }
		if err := json.Unmarshal(patch[1].Value, &taints); err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, taint := range taints {
			if taint.Effect != "NoSchedule" {
				t.Fatalf("%s: effect %q", tc.name, taint.Effect)
			}
			got = append(got, taint.Key+"="+taint.Value)
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("%s: taints=%v want %v", tc.name, got, tc.want)
		}
	}
}

func TestBackendHoldRejectsAnUnknownReason(t *testing.T) {
	runner := &scriptedRunner{outputs: map[string][]byte{}}
	if _, err := testBackend(runner).Hold(context.Background(), "worker-1", retireUID, "maintenance"); err == nil || len(runner.calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, runner.calls)
	}
}

func TestDecodeHoldRequestRequiresAnExplicitReason(t *testing.T) {
	base := `{"schemaVersion":"` + SchemaVersion + `","operation":"hold","clusterId":"cluster-a","expectedNodeName":"worker-1","nodeUid":"` + retireUID + `"`
	for _, reason := range []string{"", "paused", "quarantined", "draining", "offline"} {
		if _, err := DecodeRequest([]byte(base + `,"holdReason":"` + reason + `"}`)); err != nil {
			t.Fatalf("reason %q: %v", reason, err)
		}
	}
	for _, bad := range []string{base + `}`, base + `,"holdReason":"maintenance"}`, base + `,"holdReason":"paused","workspaceId":"x"}`} {
		if _, err := DecodeRequest([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestServiceHoldDelegatesUnderTheLock(t *testing.T) {
	backend := &fakeBackend{}
	service, err := NewService(secureTempDir(t), []byte("0123456789abcdef0123456789abcdef"), backend)
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Handle(context.Background(), Request{SchemaVersion: SchemaVersion, Operation: "hold", ClusterID: "cluster-a", ExpectedNodeName: "worker-1", NodeUID: retireUID, HoldReason: "paused"})
	hold, ok := response.(HoldResponse)
	if err != nil || !ok || !hold.Changed || hold.HoldReason != "paused" || len(backend.held) != 1 || backend.held[0] != "worker-1/"+retireUID+"/paused" {
		t.Fatalf("response=%#v err=%v held=%v", response, err, backend.held)
	}
}

package microk8sissuer

import (
	"encoding/json"
	"context"
	"errors"
	"strings"
	"testing"
)

const retireUID = "7f3c2a10-0000-4000-8000-000000000001"

func TestDecodeRetireRequiresTheExactNodeBinding(t *testing.T) {
	valid := `{"schemaVersion":"` + SchemaVersion + `","operation":"retire","clusterId":"cluster-1","expectedNodeName":"worker-1","nodeUid":"` + retireUID + `"}`
	if req, err := DecodeRequest([]byte(valid)); err != nil || req.NodeUID != retireUID {
		t.Fatalf("valid retire rejected: %v", err)
	}
	for _, body := range []string{
		strings.Replace(valid, retireUID, "not-a-uid", 1),
		strings.Replace(valid, `"worker-1"`, `"Worker_1"`, 1),
		strings.Replace(valid, `,"nodeUid":"`+retireUID+`"`, "", 1),
		strings.Replace(valid, `}`, `,"providerHandle":"`+retireUID+`"}`, 1),
	} {
		if _, err := DecodeRequest([]byte(body)); err == nil {
			t.Fatalf("invalid retire accepted: %s", body)
		}
	}
}

type scriptedRunner struct {
	outputs map[string][]byte
	calls   []string
}

func (r *scriptedRunner) Run(_ context.Context, path string, args []string) ([]byte, error) {
	call := strings.Join(args, " ")
	r.calls = append(r.calls, call)
	if out, ok := r.outputs[args[0]]; ok {
		return out, nil
	}
	return nil, errors.New("unexpected command " + path + " " + call)
}

func testBackend(runner CommandRunner) *MicroK8sBackend {
	return &MicroK8sBackend{AddNodePath: "/snap/bin/microk8s.add-node", StatusPath: "/snap/bin/microk8s.status", KubectlPath: "/snap/bin/microk8s.kubectl", Runner: runner, allowTestPaths: true}
}

func TestBackendRetireDeletesOnlyARetiredBlaznWorker(t *testing.T) {
	node := func(labels, taints string) []byte {
		return []byte(`{"metadata":{"name":"worker-1","uid":"` + retireUID + `","labels":{` + labels + `}},"spec":{"taints":[` + taints + `]}}`)
	}
	retiredTaint := `{"key":"blazn.dev/retired","value":"true","effect":"NoExecute"}`
	for _, tc := range []struct {
		name, labels, taints, uid string
		deleted                   bool
		code                      string
	}{
		{name: "retired worker", labels: `"blazn.dev/node":"true"`, taints: retiredTaint, uid: retireUID, deleted: true},
		{name: "retired worker after label rollback", labels: `"node.kubernetes.io/microk8s-worker":"microk8s-worker"`, taints: `{"key":"blazn.dev/sandbox-node","value":"true","effect":"NoSchedule"},` + retiredTaint, uid: retireUID, deleted: true},
		{name: "not yet left", labels: `"blazn.dev/node":"true"`, taints: `{"key":"blazn.dev/sandbox-node","value":"true","effect":"NoSchedule"}`, uid: retireUID, code: "retire_rejected"},
		{name: "not a Blazn node", labels: `"kubernetes.io/hostname":"worker-1"`, taints: retiredTaint, uid: retireUID, code: "retire_rejected"},
		{name: "control plane", labels: `"blazn.dev/node":"true","node-role.kubernetes.io/control-plane":""`, taints: retiredTaint, uid: retireUID, code: "retire_rejected"},
		{name: "different UID", labels: `"blazn.dev/node":"true"`, taints: retiredTaint, uid: "7f3c2a10-0000-4000-8000-000000000002", code: "binding_conflict"},
	} {
		runner := &scriptedRunner{outputs: map[string][]byte{"get": node(tc.labels, tc.taints), "delete": nil}}
		backend := testBackend(runner)
		deleted, err := backend.Retire(context.Background(), "worker-1", tc.uid)
		var protocol *ProtocolError
		if tc.code != "" {
			if !errors.As(err, &protocol) || protocol.Code != tc.code || deleted {
				t.Fatalf("%s: deleted=%v err=%v", tc.name, deleted, err)
			}
			for _, call := range runner.calls {
				if strings.HasPrefix(call, "delete") {
					t.Fatalf("%s: deleted a Node it must refuse", tc.name)
				}
			}
			continue
		}
		if err != nil || !deleted || runner.calls[len(runner.calls)-1] != "delete node worker-1 --wait=false" {
			t.Fatalf("%s: deleted=%v err=%v calls=%v", tc.name, deleted, err, runner.calls)
		}
	}
	gone := &scriptedRunner{outputs: map[string][]byte{"get": []byte("\n")}}
	if deleted, err := testBackend(gone).Retire(context.Background(), "worker-1", retireUID); err != nil || deleted {
		t.Fatalf("an absent Node must be an idempotent no-op: deleted=%v err=%v", deleted, err)
	}
}

func TestServiceRetireReportsTheBackendResult(t *testing.T) {
	backend := &fakeBackend{retireDeleted: true}
	service, err := NewService(secureTempDir(t), []byte("0123456789abcdef0123456789abcdef"), backend)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Handle(context.Background(), Request{SchemaVersion: SchemaVersion, Operation: "retire", ClusterID: "cluster-1", ExpectedNodeName: "worker-1", NodeUID: retireUID})
	response, ok := result.(RetireResponse)
	if err != nil || !ok || !response.Deleted || response.NodeUID != retireUID || len(backend.retired) != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	backend.retireErr = errors.New("private kubectl detail")
	if _, err := service.Handle(context.Background(), Request{SchemaVersion: SchemaVersion, Operation: "retire", ClusterID: "cluster-1", ExpectedNodeName: "worker-1", NodeUID: retireUID}); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("backend failure must map to a safe protocol error: %v", err)
	}
}

func TestBackendAssignReleasesAnActivatedBlaznWorkerInOnePatch(t *testing.T) {
	const workspace = "11111111-2222-4333-8444-555555555555"
	node := func(labels, taints string, unschedulable bool) []byte {
		cordon := ""
		if unschedulable {
			cordon = `"unschedulable":true,`
		}
		return []byte(`{"metadata":{"name":"worker-1","uid":"` + retireUID + `","resourceVersion":"41","labels":{` + labels + `}},"spec":{` + cordon + `"taints":[` + taints + `]}}`)
	}
	sandboxTaint := `{"key":"blazn.dev/sandbox-node","value":"true","effect":"NoSchedule"}`
	bootstrap := `,{"key":"blazn.dev/bootstrap","value":"pending","effect":"NoSchedule"}`
	hold := `,{"key":"` + PlacementHoldTaint + `","value":"paused","effect":"NoSchedule"}`
	bound := `"blazn.dev/node":"true","` + WorkspaceNodeLabel + `":"` + workspace + `"`
	eligible := `,"` + SandboxEligibleLabel + `":"true"`
	for _, tc := range []struct {
		name, labels, taints, uid string
		unschedulable, assigned   bool
		wantTaints                string
		code                      string
	}{
		{name: "freshly joined worker", labels: `"blazn.dev/node":"true"`, taints: sandboxTaint + bootstrap, uid: retireUID, assigned: true, wantTaints: "blazn.dev/sandbox-node"},
		{name: "keeps an unrelated hold", labels: `"blazn.dev/node":"true"`, taints: sandboxTaint + bootstrap + hold, uid: retireUID, assigned: true, wantTaints: "blazn.dev/sandbox-node," + PlacementHoldTaint},
		{name: "already released here", labels: bound + eligible, taints: sandboxTaint, uid: retireUID},
		{name: "released by an older node agent", labels: `"blazn.dev/node":"true"` + eligible, taints: sandboxTaint, uid: retireUID, assigned: true, wantTaints: "blazn.dev/sandbox-node"},
		{name: "cordoned", labels: bound + eligible, taints: sandboxTaint, unschedulable: true, uid: retireUID, assigned: true, wantTaints: "blazn.dev/sandbox-node"},
		{name: "bound elsewhere", labels: `"` + WorkspaceNodeLabel + `":"99999999-2222-4333-8444-555555555555"`, taints: sandboxTaint + bootstrap, uid: retireUID, code: "binding_conflict"},
		{name: "conflicting eligibility", labels: `"blazn.dev/node":"true","` + SandboxEligibleLabel + `":"false"`, taints: sandboxTaint + bootstrap, uid: retireUID, code: "assign_rejected"},
		{name: "not a Blazn node", labels: `"kubernetes.io/hostname":"worker-1"`, uid: retireUID, code: "assign_rejected"},
		{name: "retired", labels: `"blazn.dev/node":"true"`, taints: sandboxTaint + `,{"key":"blazn.dev/retired","value":"true","effect":"NoExecute"}`, uid: retireUID, code: "assign_rejected"},
		{name: "control plane", labels: `"blazn.dev/node":"true","node-role.kubernetes.io/control-plane":""`, taints: sandboxTaint, uid: retireUID, code: "assign_rejected"},
		{name: "MicroK8s control plane", labels: `"blazn.dev/node":"true","node.kubernetes.io/microk8s-controlplane":"microk8s-controlplane"`, taints: sandboxTaint, uid: retireUID, code: "assign_rejected"},
		{name: "different UID", labels: `"blazn.dev/node":"true"`, taints: sandboxTaint + bootstrap, uid: "7f3c2a10-0000-4000-8000-000000000002", code: "binding_conflict"},
	} {
		runner := &scriptedRunner{outputs: map[string][]byte{"get": node(tc.labels, tc.taints, tc.unschedulable), "patch": nil}}
		assigned, err := testBackend(runner).Assign(context.Background(), "worker-1", tc.uid, workspace)
		var protocol *ProtocolError
		if tc.code != "" {
			if !errors.As(err, &protocol) || protocol.Code != tc.code || assigned || len(runner.calls) != 1 {
				t.Fatalf("%s: assigned=%v err=%v calls=%v", tc.name, assigned, err, runner.calls)
			}
			continue
		}
		if err != nil || assigned != tc.assigned {
			t.Fatalf("%s: assigned=%v err=%v", tc.name, assigned, err)
		}
		if !tc.assigned {
			if len(runner.calls) != 1 {
				t.Fatalf("%s: a released Node was patched: %v", tc.name, runner.calls)
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
		if err := json.Unmarshal([]byte(strings.TrimPrefix(runner.calls[1], prefix)), &patch); err != nil || len(patch) != 4 ||
			patch[0].Op != "test" || patch[0].Path != "/metadata/resourceVersion" || string(patch[0].Value) != `"41"` ||
			patch[1].Path != "/metadata/labels" || patch[2].Path != "/spec/taints" || patch[3].Path != "/spec/unschedulable" || string(patch[3].Value) != "false" {
			t.Fatalf("%s: patch=%s", tc.name, runner.calls[1])
		}
		var labels map[string]string
		var taints []struct{ Key string }
		if json.Unmarshal(patch[1].Value, &labels) != nil || json.Unmarshal(patch[2].Value, &taints) != nil {
			t.Fatalf("%s: undecodable patch", tc.name)
		}
		if labels[WorkspaceNodeLabel] != workspace || labels[SandboxEligibleLabel] != "true" || labels["blazn.dev/node"] != "true" {
			t.Fatalf("%s: labels=%v", tc.name, labels)
		}
		keys := []string{}
		for _, taint := range taints {
			keys = append(keys, taint.Key)
		}
		if strings.Join(keys, ",") != tc.wantTaints {
			t.Fatalf("%s: taints=%v want %s", tc.name, keys, tc.wantTaints)
		}
	}
}

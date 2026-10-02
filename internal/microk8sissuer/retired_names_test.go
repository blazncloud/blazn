package microk8sissuer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func registrationMap(retired string) []byte {
	return []byte(`{"metadata":{"name":"blazn-node-registration","resourceVersion":"7"},"data":{"frontroHosts":"ben1,ben2,ben3","retiredNodeNames":"` + retired + `"}}`)
}

func retiredNamesPatch(t *testing.T, calls []string) (string, bool) {
	t.Helper()
	prefix := "patch configmap blazn-node-registration -n blazn-test --type=json -p "
	for _, call := range calls {
		if !strings.HasPrefix(call, prefix) {
			continue
		}
		var patch []struct {
			Op, Path string
			Value    json.RawMessage
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(call, prefix)), &patch); err != nil || len(patch) != 2 ||
			patch[0].Op != "test" || patch[0].Path != "/metadata/resourceVersion" || string(patch[0].Value) != `"7"` || patch[1].Path != "/data/retiredNodeNames" {
			t.Fatalf("registration patch=%s", call)
		}
		var value string
		_ = json.Unmarshal(patch[1].Value, &value)
		return value, true
	}
	return "", false
}

func TestRetireListsTheNameBeforeDeletingTheNode(t *testing.T) {
	node := []byte(`{"metadata":{"name":"worker-1","uid":"` + retireUID + `","labels":{"blazn.dev/node":"true"}},"spec":{"taints":[{"key":"blazn.dev/retired","value":"true","effect":"NoExecute"}]}}`)
	runner := &scriptedRunner{outputs: map[string][]byte{"get": node, "get configmap": registrationMap("worker-0"), "delete": nil}}
	if deleted, err := testBackend(runner).Retire(context.Background(), "worker-1", retireUID); err != nil || !deleted {
		t.Fatalf("deleted=%v err=%v", deleted, err)
	}
	value, patched := retiredNamesPatch(t, runner.calls)
	if !patched || value != "worker-0,worker-1" {
		t.Fatalf("retired names=%q patched=%v", value, patched)
	}
	patchAt, deleteAt := -1, -1
	for index, call := range runner.calls {
		if strings.HasPrefix(call, "patch configmap") {
			patchAt = index
		}
		if strings.HasPrefix(call, "delete node") {
			deleteAt = index
		}
	}
	if patchAt < 0 || deleteAt < patchAt {
		t.Fatalf("the name was not listed before the Node was deleted: %v", runner.calls)
	}
}

func TestRetireNeverListsAFrontroHostAndARefusedRetireListsNothing(t *testing.T) {
	retired := []byte(`{"metadata":{"name":"ben3","uid":"` + retireUID + `","labels":{"blazn.dev/node":"true"}},"spec":{"taints":[{"key":"blazn.dev/retired","value":"true","effect":"NoExecute"}]}}`)
	runner := &scriptedRunner{outputs: map[string][]byte{"get": retired, "get configmap": registrationMap(""), "delete": nil}}
	var protocol *ProtocolError
	if _, err := testBackend(runner).Retire(context.Background(), "ben3", retireUID); !errors.As(err, &protocol) || protocol.Code != "retire_rejected" {
		t.Fatalf("err=%v", err)
	}
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "patch configmap") || strings.HasPrefix(call, "delete") {
			t.Fatalf("a Frontro host was listed or deleted: %v", runner.calls)
		}
	}
	notRetired := []byte(`{"metadata":{"name":"worker-1","uid":"` + retireUID + `","labels":{"blazn.dev/node":"true"}},"spec":{"taints":[]}}`)
	runner = &scriptedRunner{outputs: map[string][]byte{"get": notRetired, "get configmap": registrationMap("")}}
	if _, err := testBackend(runner).Retire(context.Background(), "worker-1", retireUID); !errors.As(err, &protocol) || protocol.Code != "retire_rejected" {
		t.Fatalf("err=%v", err)
	}
	if _, patched := retiredNamesPatch(t, runner.calls); patched {
		t.Fatal("a refused retire listed the name")
	}
}

func TestClearRetiredNameRemovesOnlyThatNameAndToleratesAMissingList(t *testing.T) {
	runner := &scriptedRunner{outputs: map[string][]byte{"get configmap": registrationMap("worker-0,worker-1,worker-2")}}
	if err := testBackend(runner).ClearRetiredName(context.Background(), "worker-1"); err != nil {
		t.Fatal(err)
	}
	if value, patched := retiredNamesPatch(t, runner.calls); !patched || value != "worker-0,worker-2" {
		t.Fatalf("retired names=%q patched=%v", value, patched)
	}
	runner = &scriptedRunner{outputs: map[string][]byte{"get configmap": registrationMap("worker-0")}}
	if err := testBackend(runner).ClearRetiredName(context.Background(), "worker-1"); err != nil || len(runner.calls) != 1 {
		t.Fatalf("an absent name was patched: err=%v calls=%v", err, runner.calls)
	}
	runner = &scriptedRunner{outputs: map[string][]byte{}}
	if err := testBackend(runner).ClearRetiredName(context.Background(), "worker-1"); err != nil || len(runner.calls) != 1 {
		t.Fatalf("a cluster without the ConfigMap failed: err=%v calls=%v", err, runner.calls)
	}
}

func TestIssueClearsTheRetiredNameBeforeIssuing(t *testing.T) {
	backend := &fakeBackend{}
	service, err := NewService(secureTempDir(t), []byte("0123456789abcdef0123456789abcdef"), backend)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = service.Handle(context.Background(), Request{SchemaVersion: SchemaVersion, Operation: "issue", IssuanceID: "11111111-1111-4111-8111-111111111111",
		ClusterID: "cluster-a", ExpectedNodeName: "worker-1", BootstrapTaint: BootstrapTaint, TTLSeconds: 60, WorkerOnly: true})
	if len(backend.cleared) != 1 || backend.cleared[0] != "worker-1" {
		t.Fatalf("cleared=%v", backend.cleared)
	}
}

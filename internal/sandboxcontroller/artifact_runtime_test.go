package sandboxcontroller

import (
	"context"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
	"github.com/blazncloud/blazn/internal/sandboxio"
)

type fakeArtifactTransport struct {
	bodies map[string][]byte
	calls  int
}

func (t *fakeArtifactTransport) Exec(ctx context.Context, _ sandboxio.FrozenPodTarget, _ []string, input io.Reader, output io.Writer) error {
	t.calls++
	_, body, err := sandboxio.DecodeRequest(ctx, input, sandboxio.OperationArtifact)
	if err != nil {
		return err
	}
	request, err := sandboxio.DecodeArtifactRequest(body)
	if err != nil {
		return err
	}
	value, ok := t.bodies[request.Path]
	if !ok {
		return sandboxio.EncodeResponse(output, sandboxio.ErrorHeader(sandboxio.OperationArtifact, "artifact_not_found"), nil)
	}
	return sandboxio.EncodeResponse(output, sandboxio.SuccessHeader(sandboxio.OperationArtifact, value), value)
}

type fakeArtifactObjects struct {
	objects map[string]ArtifactObjectHead
	puts    int
}

func (f *fakeArtifactObjects) Put(_ context.Context, spec ArtifactObjectSpec, body []byte) (bool, error) {
	f.puts++
	if _, ok := f.objects[spec.Key]; ok {
		return false, nil
	}
	f.objects[spec.Key] = ArtifactObjectHead{Size: int64(len(body)), DigestSize: int64(len(body)), MediaType: spec.MediaType,
		Digest: spec.Digest, WorkspaceID: spec.WorkspaceID, SandboxID: spec.SandboxID, Name: spec.Name}
	return true, nil
}
func (f *fakeArtifactObjects) Head(_ context.Context, spec ArtifactObjectSpec) (ArtifactObjectHead, bool, error) {
	value, ok := f.objects[spec.Key]
	return value, ok, nil
}

func TestKubernetesArtifactRuntimeExportsBeforeCleanupAndAdoptsPersistedRows(t *testing.T) {
	item, observation := artifactRuntimeFixture(t)
	item.Artifacts = []Artifact{
		{Name: "optional", Path: "/workspace/artifacts/optional.txt", MediaType: "text/plain", Required: false},
		{Name: "result", Path: "/workspace/artifacts/result.txt", MediaType: "text/plain", Required: true},
	}
	transport := &fakeArtifactTransport{bodies: map[string][]byte{"/workspace/artifacts/result.txt": []byte("result\n")}}
	owners := &sourceOwnerChecks{}
	controller, err := sandboxio.NewController(sandboxio.ControllerConfig{Transport: transport, Owners: owners, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	objects := &fakeArtifactObjects{objects: map[string]ArtifactObjectHead{}}
	runtime, err := NewKubernetesArtifactRuntime(KubernetesArtifactRuntimeConfig{IO: controller, Objects: objects})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Export(context.Background(), item, observation)
	if err != nil || len(result.Artifacts) != 1 || result.Artifacts[0].Name != "result" || result.Artifacts[0].ID != "" ||
		!reflect.DeepEqual(result.WarningCodes, []string{"optional_artifact_missing:optional"}) || objects.puts != 1 || transport.calls != 2 {
		t.Fatalf("result=%#v puts=%d reads=%d err=%v", result, objects.puts, transport.calls, err)
	}
	persisted := result.Artifacts[0]
	persisted.ID = "80000000-0000-4000-8000-000000000001"
	item.PersistedArtifacts = []PersistedArtifact{persisted}
	second, err := runtime.Export(context.Background(), item, observation)
	if err != nil || len(second.Artifacts) != 1 || second.Artifacts[0].ID != persisted.ID || objects.puts != 1 || transport.calls != 3 {
		t.Fatalf("adoption=%#v puts=%d reads=%d err=%v", second, objects.puts, transport.calls, err)
	}
	objects.objects[persisted.ObjectKey] = ArtifactObjectHead{Size: persisted.Size, DigestSize: persisted.Size, MediaType: "application/octet-stream"}
	if _, err := runtime.Export(context.Background(), item, observation); err == nil {
		t.Fatal("mismatched persisted object was adopted")
	}
}

func artifactRuntimeFixture(t *testing.T) (WorkItem, sandboxcontrol.AdmissionObservation) {
	t.Helper()
	workspaceID, sandboxID := "40000000-0000-4000-8000-000000000001", "30000000-0000-4000-8000-000000000001"
	record := sandboxcontrol.SandboxRecord{Name: sandboxID, Namespace: sandboxcontrol.Namespace, UID: "sandbox-uid", ResourceVersion: "sandbox-rv",
		WorkspaceID: workspaceID, OwnerID: "owner-1", QueueName: sandboxcontrol.QueueName, State: sandboxcontrol.StateReady,
		ArtifactContractDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	identity := sandboxcontrol.WorkloadIdentity{APIVersion: sandboxcontrol.AdmissionAPIVersion, Namespace: sandboxcontrol.Namespace,
		Name: "workload.sandbox", UID: "workload-uid", ResourceVersion: "workload-rv", ClusterQueue: "poc-cluster",
		Owner:       sandboxcontrol.SandboxOwnerReference{APIVersion: sandboxcontrol.APIVersion, Kind: sandboxcontrol.Kind, Name: sandboxID, UID: record.UID, Controller: true},
		WorkspaceID: workspaceID, SandboxID: sandboxID, Admitted: true, Condition: sandboxcontrol.AdmissionCondition{Type: "Admitted", Status: "True"}}
	receipt, err := sandboxcontrol.NewReceipt("artifact-runtime", sandboxcontrol.OperationCreate, record, nil, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err = sandboxcontrol.AttachAdmissionIdentity(receipt, identity)
	if err != nil {
		t.Fatal(err)
	}
	observation := sandboxcontrol.AdmissionObservation{Sandbox: sandboxcontrol.ObjectIdentity{APIVersion: sandboxcontrol.APIVersion, Kind: sandboxcontrol.Kind,
		Namespace: sandboxcontrol.Namespace, Name: sandboxID, UID: record.UID, ResourceVersion: record.ResourceVersion},
		Pod:      sandboxcontrol.ObjectIdentity{APIVersion: "v1", Kind: "Pod", Namespace: sandboxcontrol.Namespace, Name: "pod.sandbox", UID: "pod-uid", ResourceVersion: "pod-rv"},
		Workload: *receipt.Admission}
	observation.Digest = sandboxcontrol.AdmissionObservationDigest(observation)
	item := WorkItem{OperationID: "operation-123", WorkspaceID: workspaceID, SandboxID: sandboxID, RequestedBy: "owner-1",
		BackendUID: &observation.Sandbox.UID, BackendResourceVersion: &observation.Sandbox.ResourceVersion, AdmissionObservation: &observation}
	return item, observation
}

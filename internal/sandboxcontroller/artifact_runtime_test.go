package sandboxcontroller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	bodies  map[string][]byte
	getErr  error
	missing bool
	puts    int
}

func (f *fakeArtifactObjects) Put(_ context.Context, spec ArtifactObjectSpec, body []byte) (bool, error) {
	f.puts++
	if _, ok := f.objects[spec.Key]; ok {
		return false, nil
	}
	f.objects[spec.Key] = ArtifactObjectHead{Size: int64(len(body)), DigestSize: int64(len(body)), MediaType: spec.MediaType,
		Digest: spec.Digest, WorkspaceID: spec.WorkspaceID, SandboxID: spec.SandboxID, Name: spec.Name}
	f.bodies[spec.Key] = append([]byte(nil), body...)
	return true, nil
}
func (f *fakeArtifactObjects) Get(_ context.Context, spec ArtifactObjectSpec) ([]byte, bool, error) {
	if f.getErr != nil {
		return nil, false, f.getErr
	}
	if f.missing {
		return nil, false, nil
	}
	value, ok := f.bodies[spec.Key]
	return append([]byte(nil), value...), ok, nil
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
	objects := &fakeArtifactObjects{objects: map[string]ArtifactObjectHead{}, bodies: map[string][]byte{}}
	runtime, err := NewKubernetesArtifactRuntime(KubernetesArtifactRuntimeConfig{IO: controller, Objects: objects})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Export(context.Background(), item, observation)
	if err != nil || len(result.Artifacts) != 1 || result.Artifacts[0].Name != "result" || result.Artifacts[0].ID != "" ||
		!reflect.DeepEqual(result.WarningCodes, []string{"optional_artifact_missing_optional"}) || objects.puts != 1 || transport.calls != 2 {
		t.Fatalf("result=%#v puts=%d reads=%d err=%v", result, objects.puts, transport.calls, err)
	}
	persisted := result.Artifacts[0]
	persisted.ID = "80000000-0000-4000-8000-000000000001"
	persisted.ExportedAt = "2026-08-24T12:00:00Z"
	item.PersistedArtifacts = []PersistedArtifact{persisted}
	second, err := runtime.Export(context.Background(), item, observation)
	if err != nil || len(second.Artifacts) != 1 || second.Artifacts[0].ID != persisted.ID || objects.puts != 1 || transport.calls != 3 {
		t.Fatalf("adoption=%#v puts=%d reads=%d err=%v", second, objects.puts, transport.calls, err)
	}
	objects.bodies[persisted.ObjectKey] = []byte("forged\n")
	if _, err := runtime.Export(context.Background(), item, observation); err == nil {
		t.Fatal("persisted artifact with matching metadata and wrong object bytes was adopted")
	}
	objects.bodies[persisted.ObjectKey] = []byte("result\n")
	objects.objects[persisted.ObjectKey] = ArtifactObjectHead{Size: persisted.Size, DigestSize: persisted.Size, MediaType: "application/octet-stream"}
	if _, err := runtime.Export(context.Background(), item, observation); err == nil {
		t.Fatal("mismatched persisted object was adopted")
	}
}

func TestKubernetesArtifactRuntimeVerifiesExistingObjectBytesBeforeAdoption(t *testing.T) {
	for _, test := range []struct {
		name    string
		body    []byte
		getErr  error
		missing bool
		wantErr bool
	}{
		{name: "identical content", body: []byte("result\n")},
		{name: "forged matching metadata and size", body: []byte("forged\n"), wantErr: true},
		{name: "truncated read", body: []byte("result"), wantErr: true},
		{name: "oversized read", body: make([]byte, maxArtifactBytes+1), wantErr: true},
		{name: "missing object", missing: true, wantErr: true},
		{name: "store error", getErr: errors.New("store unavailable"), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			item, observation := artifactRuntimeFixture(t)
			item.Artifacts = []Artifact{{Name: "result", Path: "/workspace/artifacts/result.txt", MediaType: "text/plain", Required: true}}
			transport := &fakeArtifactTransport{bodies: map[string][]byte{"/workspace/artifacts/result.txt": []byte("result\n")}}
			controller, err := sandboxio.NewController(sandboxio.ControllerConfig{Transport: transport, Owners: &sourceOwnerChecks{}, Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			key, _ := ArtifactObjectKey(item.WorkspaceID, item.SandboxID, "result")
			digest := sha256.Sum256([]byte("result\n"))
			objects := &fakeArtifactObjects{objects: map[string]ArtifactObjectHead{key: {
				Size: 7, DigestSize: 7, MediaType: "text/plain", Digest: "sha256:" + hex.EncodeToString(digest[:]),
				WorkspaceID: item.WorkspaceID, SandboxID: item.SandboxID, Name: "result",
			}}, bodies: map[string][]byte{key: test.body}, getErr: test.getErr, missing: test.missing}
			runtime, err := NewKubernetesArtifactRuntime(KubernetesArtifactRuntimeConfig{IO: controller, Objects: objects})
			if err != nil {
				t.Fatal(err)
			}
			result, err := runtime.Export(context.Background(), item, observation)
			if (err != nil) != test.wantErr || !test.wantErr && len(result.Artifacts) != 1 || objects.puts != 1 {
				t.Fatalf("result=%#v puts=%d err=%v", result, objects.puts, err)
			}
			if !reflect.DeepEqual(objects.bodies[key], test.body) {
				t.Fatal("pre-existing object was modified")
			}
		})
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

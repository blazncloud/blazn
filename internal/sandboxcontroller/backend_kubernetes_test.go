package sandboxcontroller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
)

type fakeSandboxAdapter struct {
	calls       []string
	request     sandboxcontrol.CreateRequest
	record      sandboxcontrol.SandboxRecord
	observation sandboxcontrol.AdmissionObservation
	ensureErr   error
	observeErr  error
	deleteErr   error
	getErr      error
	finalizeErr error
	absenceErrs []error
	block       bool
}

func (f *fakeSandboxAdapter) EnsureCreated(ctx context.Context, request sandboxcontrol.CreateRequest, expectedUID string) (sandboxcontrol.SandboxRecord, sandboxcontrol.OperationReceipt, error) {
	f.calls = append(f.calls, "ensure")
	f.request = request
	if f.block {
		<-ctx.Done()
		return sandboxcontrol.SandboxRecord{}, sandboxcontrol.OperationReceipt{}, ctx.Err()
	}
	if f.ensureErr != nil {
		return sandboxcontrol.SandboxRecord{}, sandboxcontrol.OperationReceipt{}, f.ensureErr
	}
	return f.record, operationReceipt(request.RequestID, sandboxcontrol.OperationCreate, f.record, nil), nil
}

func (f *fakeSandboxAdapter) Get(context.Context, string, string, string) (sandboxcontrol.SandboxRecord, error) {
	f.calls = append(f.calls, "get")
	return f.record, f.getErr
}

func (f *fakeSandboxAdapter) ObserveAdmission(ctx context.Context, request sandboxcontrol.CreateRequest, _ sandboxcontrol.SandboxRecord, _ *sandboxcontrol.AdmissionObservation) (sandboxcontrol.AdmissionObservation, error) {
	f.calls = append(f.calls, "observe")
	f.request = request
	if f.block {
		<-ctx.Done()
		return sandboxcontrol.AdmissionObservation{}, ctx.Err()
	}
	return f.observation, f.observeErr
}

func (f *fakeSandboxAdapter) Delete(_ context.Context, requestID, _, _, _, _, _, _ string) (sandboxcontrol.OperationReceipt, error) {
	f.calls = append(f.calls, "delete")
	return operationReceipt(requestID, sandboxcontrol.OperationDelete, f.record, nil), f.deleteErr
}

func (f *fakeSandboxAdapter) Finalize(_ context.Context, requestID, _, _, _, _, _ string, _ []sandboxcontrol.ArtifactExport, _ string) (sandboxcontrol.OperationReceipt, error) {
	f.calls = append(f.calls, "finalize")
	return operationReceipt(requestID, sandboxcontrol.OperationFinalize, f.record, nil), f.finalizeErr
}

func (f *fakeSandboxAdapter) ObserveAbsence(context.Context, sandboxcontrol.AdmissionObservation) error {
	f.calls = append(f.calls, "absence")
	if len(f.absenceErrs) == 0 {
		return nil
	}
	err := f.absenceErrs[0]
	f.absenceErrs = f.absenceErrs[1:]
	return err
}

func TestKubernetesBackendMapsExactApprovedCreateRequest(t *testing.T) {
	item, record, observation := backendFixture(t)
	fake := &fakeSandboxAdapter{record: record, observation: observation}
	backend := newTestKubernetesBackend(t, fake, true)
	state, err := backend.EnsureCreated(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	wantArtifacts := []sandboxcontrol.ArtifactExport{{Name: "result", Path: "/workspace/artifacts/result.json", MediaType: "application/json", Required: true}}
	want := sandboxcontrol.CreateRequest{RequestID: "controller-" + item.OperationID, Name: item.SandboxID,
		WorkspaceID: item.WorkspaceID, OwnerID: item.RequestedBy, Image: item.ImageDigest,
		Command: []string{"/bin/true"}, Architecture: "amd64", RuntimeClassName: "",
		TrustLevel: sandboxcontrol.TrustApprovedPOC, NonSensitive: true,
		CPURequest: "100m", MemoryRequest: "128Mi", EphemeralStorageRequest: "1Gi",
		CPULimit: "1", MemoryLimit: "1Gi", EphemeralStorageLimit: "2Gi",
		ExpiresAt: item.ExpiresAt, Artifacts: wantArtifacts}
	if !reflect.DeepEqual(fake.request, want) {
		t.Fatalf("create mapping mismatch:\n got %#v\nwant %#v", fake.request, want)
	}
	if !state.Exists || state.Ready || state.Admission != nil {
		t.Fatalf("creation claimed admission before observation: %#v", state)
	}
	observed, err := backend.Observe(context.Background(), item)
	if err != nil || !observed.Ready || observed.AdmissionObservation == nil || !reflect.DeepEqual(*observed.Admission, observation.Workload) {
		t.Fatalf("full admission evidence was not returned: state=%#v err=%v", observed, err)
	}
}

func TestKubernetesBackendRefusesSourcesAndUnsupportedRequiredArtifactsBeforeMutation(t *testing.T) {
	item, record, observation := backendFixture(t)
	for _, test := range []struct {
		name   string
		mutate func(*WorkItem)
	}{
		{name: "source", mutate: func(item *WorkItem) {
			item.Sources = []Source{{Name: "repo", URL: "https://example.invalid/repo", Destination: "/workspace/src/repo", Commit: strings.Repeat("a", 40)}}
		}},
		{name: "required artifact", mutate: func(item *WorkItem) {}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := item
			test.mutate(&candidate)
			fake := &fakeSandboxAdapter{record: record, observation: observation}
			backend := newTestKubernetesBackend(t, fake, false)
			_, err := backend.EnsureCreated(context.Background(), candidate)
			if err == nil || len(fake.calls) != 0 {
				t.Fatalf("refusal mutated backend: calls=%v err=%v", fake.calls, err)
			}
		})
	}
}

func TestKubernetesBackendCleanupDeletesFinalizesAndProvesExactAbsence(t *testing.T) {
	item, record, observation := backendFixture(t)
	bindBackendIdentity(&item, record, observation.Workload)
	record.Deleting = true
	record.Finalizers = []string{sandboxcontrol.CleanupFinalizer}
	fake := &fakeSandboxAdapter{record: record, observation: observation,
		absenceErrs: []error{&sandboxcontrol.AdapterError{Code: sandboxcontrol.ErrCleanupIncomplete, Status: 409, SafeDetail: "still deleting"}, nil}}
	backend := newTestKubernetesBackend(t, fake, true)
	state, err := backend.BeginDelete(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	result, err := backend.Finalize(context.Background(), item, state)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fake.calls, []string{"observe", "delete", "get", "finalize", "absence", "absence"}) {
		t.Fatalf("cleanup sequence=%v", fake.calls)
	}
	if !result.CleanupComplete || !result.ArtifactExportComplete || !result.GrantsRevoked || !result.BackendDestroyed {
		t.Fatalf("incomplete cleanup result: %#v", result)
	}
}

func TestKubernetesBackendCleanupRestartAndAlreadyDeletedFailClosed(t *testing.T) {
	item, record, observation := backendFixture(t)
	bindBackendIdentity(&item, record, observation.Workload)
	backend := newTestKubernetesBackend(t, &fakeSandboxAdapter{}, true)
	_, err := backend.Finalize(context.Background(), item, BackendState{})
	assertAmbiguousRetryable(t, err)

	fake := &fakeSandboxAdapter{observeErr: &sandboxcontrol.AdapterError{Code: sandboxcontrol.ErrNotFound, Status: 404, SafeDetail: "secret-backend-material"}}
	backend = newTestKubernetesBackend(t, fake, true)
	_, err = backend.BeginDelete(context.Background(), item)
	assertAmbiguousRetryable(t, err)
	if strings.Contains(err.Error(), "secret-backend-material") || !reflect.DeepEqual(fake.calls, []string{"observe"}) {
		t.Fatalf("already-deleted cleanup leaked or mutated: calls=%v err=%v", fake.calls, err)
	}
}

func TestKubernetesBackendClassifiesTransportIdentityAndResidueWithoutLeaks(t *testing.T) {
	item, record, observation := backendFixture(t)
	secret := "bearer-do-not-log"
	for _, test := range []struct {
		name      string
		err       error
		retryable bool
		ambiguous bool
	}{
		{name: "transport", err: errors.New(secret), retryable: true},
		{name: "five hundred", err: &sandboxcontrol.AdapterError{Code: sandboxcontrol.ErrBackend, Status: 503, SafeDetail: secret}, retryable: true},
		{name: "substitution", err: &sandboxcontrol.AdapterError{Code: sandboxcontrol.ErrConflict, Status: 409, SafeDetail: secret}, ambiguous: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeSandboxAdapter{record: record, observation: observation, ensureErr: test.err}
			backend := newTestKubernetesBackend(t, fake, true)
			_, err := backend.EnsureCreated(context.Background(), item)
			failure, ok := BackendFailure(err)
			if !ok || failure.Retryable != test.retryable || failure.Ambiguous != test.ambiguous || strings.Contains(err.Error(), secret) {
				t.Fatalf("classification=%#v err=%v", failure, err)
			}
		})
	}
}

func TestKubernetesBackendHonorsCancellationAndHealthIsReadOnly(t *testing.T) {
	item, _, _ := backendFixture(t)
	fake := &fakeSandboxAdapter{block: true}
	healthCalls := 0
	backend, err := NewKubernetesBackend(KubernetesBackendConfig{Adapter: fake,
		Health: func(context.Context) error { healthCalls++; return nil }, AbsencePollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Health(context.Background()); err != nil || healthCalls != 1 || len(fake.calls) != 0 {
		t.Fatalf("health mutated adapter: health=%d calls=%v err=%v", healthCalls, fake.calls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = backend.EnsureCreated(ctx, item)
	if !errors.Is(err, context.Canceled) || len(fake.calls) != 0 {
		t.Fatalf("canceled create reached adapter: calls=%v err=%v", fake.calls, err)
	}
}

func TestValidateWorkItemRequiresCanonicalImmutableImageReferences(t *testing.T) {
	item, _, _ := backendFixture(t)
	for _, image := range []string{
		"sha256:" + strings.Repeat("a", 64),
		"registry.invalid/poc:latest",
		"registry.invalid/poc@sha256:" + strings.Repeat("A", 64),
	} {
		candidate := item
		candidate.ImageDigest = image
		if err := validateWorkItem(candidate); err == nil {
			t.Fatalf("invalid child image reference accepted: %q", image)
		}
	}
	if err := validateWorkItem(item); err != nil {
		t.Fatalf("canonical image references rejected: %v", err)
	}
}

func newTestKubernetesBackend(t *testing.T, adapter SandboxControlAdapter, artifacts bool) *KubernetesBackend {
	t.Helper()
	backend, err := NewKubernetesBackend(KubernetesBackendConfig{Adapter: adapter,
		Health: func(context.Context) error { return nil }, ArtifactExportSupported: artifacts,
		AbsencePollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func backendFixture(t *testing.T) (WorkItem, sandboxcontrol.SandboxRecord, sandboxcontrol.AdmissionObservation) {
	t.Helper()
	item, state := validItemAndState(t)
	item.BackendUID, item.BackendResourceVersion, item.AdmissionID, item.Admission = nil, nil, nil, nil
	item.Artifacts = []Artifact{{Name: "result", Path: "/workspace/artifacts/result.json", MediaType: "application/json", Required: true}}
	artifacts := []sandboxcontrol.ArtifactExport{{Name: "result", Path: "/workspace/artifacts/result.json", MediaType: "application/json", Required: true}}
	canonical, digest, err := sandboxcontrol.CanonicalArtifactContract(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	record := state.Record
	record.Artifacts, record.ArtifactContractDigest = canonical, digest
	observation := sandboxcontrol.AdmissionObservation{
		Sandbox:  sandboxcontrol.ObjectIdentity{APIVersion: sandboxcontrol.APIVersion, Kind: sandboxcontrol.Kind, Namespace: sandboxcontrol.Namespace, Name: item.SandboxID, UID: record.UID, ResourceVersion: record.ResourceVersion},
		Pod:      sandboxcontrol.ObjectIdentity{APIVersion: "v1", Kind: "Pod", Namespace: sandboxcontrol.Namespace, Name: "pod-1", UID: "pod-uid", ResourceVersion: "pod-rv"},
		Workload: *state.Admission, Digest: "sha256:" + strings.Repeat("f", 64),
	}
	return item, record, observation
}

func bindBackendIdentity(item *WorkItem, record sandboxcontrol.SandboxRecord, identity sandboxcontrol.WorkloadIdentity) {
	item.BackendUID = pointer(record.UID)
	item.BackendResourceVersion = pointer(record.ResourceVersion)
	item.AdmissionID = pointer(identity.UID)
	item.Admission = &identity
}

func operationReceipt(requestID string, operation sandboxcontrol.Operation, record sandboxcontrol.SandboxRecord, artifacts []sandboxcontrol.ArtifactReceipt) sandboxcontrol.OperationReceipt {
	record.State = map[sandboxcontrol.Operation]sandboxcontrol.SandboxState{sandboxcontrol.OperationDelete: sandboxcontrol.StateStopping, sandboxcontrol.OperationFinalize: sandboxcontrol.StateDeleted}[operation]
	if operation == sandboxcontrol.OperationCreate {
		record.State = sandboxcontrol.StatePending
	}
	receipt, err := sandboxcontrol.NewReceipt(requestID, operation, record, artifacts, time.Unix(1, 0))
	if err != nil {
		panic(err)
	}
	return receipt
}

func assertAmbiguousRetryable(t *testing.T, err error) {
	t.Helper()
	failure, ok := BackendFailure(err)
	if !ok || !failure.Ambiguous || !failure.Retryable {
		t.Fatalf("wanted ambiguous retryable failure, got %#v (%v)", failure, err)
	}
}

package sandboxcontroller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
)

type fakeSandboxAdapter struct {
	mu            sync.Mutex
	calls         []string
	request       sandboxcontrol.CreateRequest
	record        sandboxcontrol.SandboxRecord
	getRecords    []sandboxcontrol.SandboxRecord
	observation   sandboxcontrol.AdmissionObservation
	ensureErr     error
	observeErr    error
	deleteErr     error
	getErr        error
	finalizeErr   error
	absenceErrs   []error
	afterFinalize func()
	block         bool
}

func (f *fakeSandboxAdapter) EnsureCreated(ctx context.Context, request sandboxcontrol.CreateRequest, expectedUID string) (sandboxcontrol.SandboxRecord, sandboxcontrol.OperationReceipt, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "ensure")
	f.request = request
	record, block, err := cloneSandboxRecord(f.record), f.block, f.ensureErr
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return sandboxcontrol.SandboxRecord{}, sandboxcontrol.OperationReceipt{}, ctx.Err()
	}
	if err != nil {
		return sandboxcontrol.SandboxRecord{}, sandboxcontrol.OperationReceipt{}, err
	}
	if expectedUID != "" && expectedUID != record.UID {
		return sandboxcontrol.SandboxRecord{}, sandboxcontrol.OperationReceipt{}, adapterConflict("create expected UID changed")
	}
	return record, operationReceipt(request.RequestID, sandboxcontrol.OperationCreate, record, nil), nil
}

func (f *fakeSandboxAdapter) Get(_ context.Context, workspaceID, ownerID, name string) (sandboxcontrol.SandboxRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "get")
	if f.getErr != nil {
		return sandboxcontrol.SandboxRecord{}, f.getErr
	}
	if workspaceID != f.record.WorkspaceID || ownerID != f.record.OwnerID || name != f.record.Name {
		return sandboxcontrol.SandboxRecord{}, &sandboxcontrol.AdapterError{Code: sandboxcontrol.ErrIdentityBoundary, Status: 404, SafeDetail: "get identity changed"}
	}
	if len(f.getRecords) != 0 {
		f.record = cloneSandboxRecord(f.getRecords[0])
		f.getRecords = f.getRecords[1:]
	}
	return cloneSandboxRecord(f.record), nil
}

func (f *fakeSandboxAdapter) ObserveAdmission(ctx context.Context, request sandboxcontrol.CreateRequest, record sandboxcontrol.SandboxRecord, expected *sandboxcontrol.AdmissionObservation) (sandboxcontrol.AdmissionObservation, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "observe")
	f.request = request
	current, observation := cloneSandboxRecord(f.record), f.observation
	block, err := f.block, f.observeErr
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return sandboxcontrol.AdmissionObservation{}, ctx.Err()
	}
	if err != nil {
		return sandboxcontrol.AdmissionObservation{}, err
	}
	if record.UID != current.UID || record.ResourceVersion != current.ResourceVersion ||
		record.ArtifactContractDigest != current.ArtifactContractDigest || !reflect.DeepEqual(record.Artifacts, current.Artifacts) {
		return sandboxcontrol.AdmissionObservation{}, adapterConflict("observe record was not exact")
	}
	if current.State != sandboxcontrol.StateReady {
		return sandboxcontrol.AdmissionObservation{}, adapterConflict("admission is not ready")
	}
	if expected != nil && !reflect.DeepEqual(*expected, observation) {
		return sandboxcontrol.AdmissionObservation{}, adapterConflict("admission observation changed")
	}
	return observation, nil
}

func (f *fakeSandboxAdapter) Delete(_ context.Context, requestID, workspaceID, ownerID, name, uid, resourceVersion, digest string) (sandboxcontrol.OperationReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "delete")
	if f.deleteErr != nil {
		return sandboxcontrol.OperationReceipt{}, f.deleteErr
	}
	if workspaceID != f.record.WorkspaceID || ownerID != f.record.OwnerID || name != f.record.Name ||
		uid != f.record.UID || resourceVersion != f.record.ResourceVersion || digest != f.record.ArtifactContractDigest || f.record.Deleting {
		return sandboxcontrol.OperationReceipt{}, adapterConflict("delete precondition changed")
	}
	receipt := operationReceipt(requestID, sandboxcontrol.OperationDelete, f.record, nil)
	f.record.Deleting = true
	f.record.State = sandboxcontrol.StateStopping
	f.record.ResourceVersion = "resource-version-delete"
	return receipt, nil
}

func (f *fakeSandboxAdapter) Finalize(_ context.Context, requestID, workspaceID, ownerID, name, uid, resourceVersion string, artifacts []sandboxcontrol.ArtifactExport, digest string) (sandboxcontrol.OperationReceipt, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "finalize")
	if f.finalizeErr != nil {
		err := f.finalizeErr
		f.mu.Unlock()
		return sandboxcontrol.OperationReceipt{}, err
	}
	if workspaceID != f.record.WorkspaceID || ownerID != f.record.OwnerID || name != f.record.Name || uid != f.record.UID ||
		resourceVersion != f.record.ResourceVersion || !f.record.Deleting || !hasFinalizer(f.record.Finalizers) ||
		digest != f.record.ArtifactContractDigest || !reflect.DeepEqual(artifacts, f.record.Artifacts) {
		f.mu.Unlock()
		return sandboxcontrol.OperationReceipt{}, adapterConflict("finalize precondition changed")
	}
	f.record.ResourceVersion = "resource-version-finalize"
	f.record.Finalizers = nil
	receipt := operationReceipt(requestID, sandboxcontrol.OperationFinalize, f.record, artifactReceipts(f.record, artifacts))
	afterFinalize := f.afterFinalize
	f.mu.Unlock()
	if afterFinalize != nil {
		afterFinalize()
	}
	return receipt, nil
}

func (f *fakeSandboxAdapter) ObserveAbsence(context.Context, sandboxcontrol.AdmissionObservation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "absence")
	if len(f.absenceErrs) == 0 {
		return nil
	}
	err := f.absenceErrs[0]
	f.absenceErrs = f.absenceErrs[1:]
	return err
}

func (f *fakeSandboxAdapter) setGetError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getErr = err
}

func (f *fakeSandboxAdapter) snapshotCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func adapterConflict(detail string) error {
	return &sandboxcontrol.AdapterError{Code: sandboxcontrol.ErrConflict, Status: 409, SafeDetail: detail}
}

func TestKubernetesBackendMapsExactApprovedCreateRequest(t *testing.T) {
	item, record, observation := backendFixture(t)
	pending := cloneSandboxRecord(record)
	pending.State = sandboxcontrol.StatePending
	ready := cloneSandboxRecord(record)
	ready.ResourceVersion = "resource-version-ready"
	observation.Sandbox.ResourceVersion = ready.ResourceVersion
	fake := &fakeSandboxAdapter{record: pending, getRecords: []sandboxcontrol.SandboxRecord{pending, ready}, observation: observation}
	backend := newTestKubernetesBackend(t, fake, true)
	state, err := backend.EnsureCreated(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	wantArtifacts := []sandboxcontrol.ArtifactExport{{Name: "result", Path: "/workspace/artifacts/result.json", MediaType: "application/json", Required: true}}
	want := sandboxcontrol.CreateRequest{RequestID: "controller-" + item.OperationID, Name: item.SandboxID,
		WorkspaceID: item.WorkspaceID, OwnerID: item.RequestedBy, Image: item.ImageDigest,
		Command: []string{"true"}, Architecture: "amd64", RuntimeClassName: "",
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
	queued, err := backend.Observe(context.Background(), item)
	if err != nil || queued.Ready || queued.Admission != nil || queued.Record.UID != record.UID || queued.Record.ArtifactContractDigest != record.ArtifactContractDigest {
		t.Fatalf("pending Sandbox was not returned as queued evidence: state=%#v err=%v", queued, err)
	}
	observed, err := backend.Observe(context.Background(), item)
	if err != nil || !observed.Ready || observed.AdmissionObservation == nil || !reflect.DeepEqual(*observed.Admission, observation.Workload) {
		t.Fatalf("full admission evidence was not returned: state=%#v err=%v", observed, err)
	}
	if observed.Record.ResourceVersion != ready.ResourceVersion || observed.Record.ArtifactContractDigest != record.ArtifactContractDigest ||
		!reflect.DeepEqual(fake.snapshotCalls(), []string{"ensure", "get", "get", "observe"}) {
		t.Fatalf("create evidence was not retained exactly: state=%#v calls=%v", observed, fake.snapshotCalls())
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
			if err == nil || len(fake.snapshotCalls()) != 0 {
				t.Fatalf("refusal mutated backend: calls=%v err=%v", fake.snapshotCalls(), err)
			}
		})
	}
}

func TestKubernetesBackendCleanupDeletesFinalizesAndProvesExactAbsence(t *testing.T) {
	item, record, observation := backendFixture(t)
	bindBackendIdentity(&item, record, observation.Workload)
	fake := &fakeSandboxAdapter{record: record, observation: observation,
		absenceErrs: []error{&sandboxcontrol.AdapterError{Code: sandboxcontrol.ErrCleanupIncomplete, Status: 409, SafeDetail: "still deleting"}, nil}}
	backend := newTestKubernetesBackend(t, fake, true)
	state, err := backend.BeginDelete(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	if state.Record.ResourceVersion == *item.BackendResourceVersion || !state.Record.Deleting || !state.CleanupFinalizerPresent {
		t.Fatalf("delete did not return the advanced deletion identity: %#v", state)
	}
	if err := validateDeleting(item, state); err != nil {
		t.Fatalf("controller rejected the exact post-delete identity: %v", err)
	}
	result, err := backend.Finalize(context.Background(), item, state)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fake.snapshotCalls(), []string{"get", "observe", "delete", "get", "finalize", "absence", "absence"}) {
		t.Fatalf("cleanup sequence=%v", fake.snapshotCalls())
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

	fake := &fakeSandboxAdapter{getErr: &sandboxcontrol.AdapterError{Code: sandboxcontrol.ErrNotFound, Status: 404, SafeDetail: "secret-backend-material"}}
	backend = newTestKubernetesBackend(t, fake, true)
	_, err = backend.BeginDelete(context.Background(), item)
	assertAmbiguousRetryable(t, err)
	if strings.Contains(err.Error(), "secret-backend-material") || !reflect.DeepEqual(fake.snapshotCalls(), []string{"get"}) {
		t.Fatalf("already-deleted cleanup leaked or mutated: calls=%v err=%v", fake.snapshotCalls(), err)
	}
}

func TestKubernetesBackendRetainsFinalizedObservationAcrossSameProcessRetry(t *testing.T) {
	item, record, observation := backendFixture(t)
	bindBackendIdentity(&item, record, observation.Workload)
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakeSandboxAdapter{record: record, observation: observation,
		absenceErrs:   []error{&sandboxcontrol.AdapterError{Code: sandboxcontrol.ErrCleanupIncomplete, Status: 409, SafeDetail: "still deleting"}},
		afterFinalize: cancel}
	backend := newTestKubernetesBackend(t, fake, true)
	state, err := backend.BeginDelete(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Finalize(ctx, item, state); !errors.Is(err, context.Canceled) {
		t.Fatalf("finalize cancellation was not preserved: %v", err)
	}
	fake.setGetError(&sandboxcontrol.AdapterError{Code: sandboxcontrol.ErrNotFound, Status: 404, SafeDetail: "gone"})
	retryState, err := backend.BeginDelete(context.Background(), item)
	if err != nil || retryState.Exists || retryState.AdmissionObservation == nil {
		t.Fatalf("same-process retry lost exact absence evidence: state=%#v err=%v", retryState, err)
	}
	result, err := backend.Finalize(context.Background(), item, retryState)
	if err != nil || !result.CleanupComplete || !result.ArtifactExportComplete || !result.GrantsRevoked || !result.BackendDestroyed {
		t.Fatalf("same-process retry did not recover the receipted cleanup: result=%#v err=%v", result, err)
	}
	wantCalls := []string{"get", "observe", "delete", "get", "finalize", "absence", "get", "absence"}
	if !reflect.DeepEqual(fake.snapshotCalls(), wantCalls) {
		t.Fatalf("same-process recovery sequence=%v", fake.snapshotCalls())
	}
	fresh := newTestKubernetesBackend(t, fake, true)
	_, err = fresh.BeginDelete(context.Background(), item)
	assertAmbiguousRetryable(t, err)
}

func TestKubernetesBackendEvidenceCacheIsConcurrent(t *testing.T) {
	item, record, observation := backendFixture(t)
	fake := &fakeSandboxAdapter{record: record, observation: observation}
	backend := newTestKubernetesBackend(t, fake, true)
	if _, err := backend.EnsureCreated(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	const observers = 24
	errorsFound := make(chan error, observers)
	var group sync.WaitGroup
	for index := 0; index < observers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			state, err := backend.Observe(context.Background(), item)
			if err == nil && (!state.Ready || state.AdmissionObservation == nil) {
				err = errors.New("concurrent observation returned incomplete evidence")
			}
			errorsFound <- err
		}()
	}
	group.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
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
	artifactErr := &sandboxcontrol.AdapterError{Code: sandboxcontrol.ErrArtifactExport, Status: 502, SafeDetail: secret}
	failure, ok := BackendFailure(classifyAdapter("cleanup", artifactErr))
	if !ok || !failure.Retryable || failure.Ambiguous || strings.Contains(failure.Error(), secret) {
		t.Fatalf("safe artifact retry classification=%#v", failure)
	}
}

func TestKubernetesBackendHonorsCancellationAndHealthIsReadOnly(t *testing.T) {
	item, _, _ := backendFixture(t)
	fake := &fakeSandboxAdapter{block: true}
	healthCalls := 0
	backend, err := NewKubernetesBackend(KubernetesBackendConfig{Adapter: fake,
		Health: func(context.Context) error { healthCalls++; return nil }, ArtifactExportSupported: true,
		AbsencePollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Health(context.Background()); err != nil || healthCalls != 1 || len(fake.snapshotCalls()) != 0 {
		t.Fatalf("health mutated adapter: health=%d calls=%v err=%v", healthCalls, fake.snapshotCalls(), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = backend.EnsureCreated(ctx, item)
	if !errors.Is(err, context.Canceled) || len(fake.snapshotCalls()) != 0 {
		t.Fatalf("canceled create reached adapter: calls=%v err=%v", fake.snapshotCalls(), err)
	}
}

func TestValidateWorkItemRequiresCanonicalImmutableImageReferences(t *testing.T) {
	item, _, _ := backendFixture(t)
	invalid := []string{
		"sha256:" + strings.Repeat("a", 64),
		"registry.invalid/poc:latest",
		"registry.invalid/poc@sha256:" + strings.Repeat("A", 64),
	}
	for _, field := range []struct {
		name   string
		mutate func(*WorkItem, string)
	}{{name: "index", mutate: func(item *WorkItem, image string) { item.ImageIndexDigest = image }},
		{name: "child", mutate: func(item *WorkItem, image string) { item.ImageDigest = image }}} {
		for _, image := range invalid {
			candidate := item
			field.mutate(&candidate, image)
			if err := validateWorkItem(candidate); err == nil {
				t.Fatalf("invalid %s image reference accepted: %q", field.name, image)
			}
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
	item, state := createFixture(t)
	item.BackendUID, item.BackendResourceVersion, item.AdmissionID, item.Admission = nil, nil, nil, nil
	item.Artifacts = []Artifact{{Name: "result", Path: "/workspace/artifacts/result.json", MediaType: "application/json", Required: true}}
	artifacts := []sandboxcontrol.ArtifactExport{{Name: "result", Path: "/workspace/artifacts/result.json", MediaType: "application/json", Required: true}}
	canonical, digest, err := sandboxcontrol.CanonicalArtifactContract(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	record := state.Record
	record.Artifacts, record.ArtifactContractDigest = canonical, digest
	record.Finalizers = []string{sandboxcontrol.CleanupFinalizer}
	record.TrustLevel = sandboxcontrol.TrustApprovedPOC
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

func artifactReceipts(record sandboxcontrol.SandboxRecord, artifacts []sandboxcontrol.ArtifactExport) []sandboxcontrol.ArtifactReceipt {
	receipts := make([]sandboxcontrol.ArtifactReceipt, 0, len(artifacts))
	for _, artifact := range artifacts {
		receipts = append(receipts, sandboxcontrol.ArtifactReceipt{SchemaVersion: sandboxcontrol.ArtifactSchema,
			Name: artifact.Name, ObjectKey: "workspaces/" + record.WorkspaceID + "/sandboxes/" + record.Name + "/artifacts/" + artifact.Name,
			SHA256: "sha256:" + strings.Repeat("e", 64), Size: 1, ExportedAt: time.Unix(2, 0).UTC().Format(time.RFC3339Nano)})
	}
	return receipts
}

func assertAmbiguousRetryable(t *testing.T, err error) {
	t.Helper()
	failure, ok := BackendFailure(err)
	if !ok || !failure.Ambiguous || !failure.Retryable {
		t.Fatalf("wanted ambiguous retryable failure, got %#v (%v)", failure, err)
	}
}

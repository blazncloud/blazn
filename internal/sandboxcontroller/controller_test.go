package sandboxcontroller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
)

type fakeStore struct {
	bound      bool
	claims     int
	bindRecord sandboxcontrol.SandboxRecord
	completion *Completion
	retry      *SafeError
}

func (s *fakeStore) Claim(context.Context, string, int) (*WorkItem, error) {
	s.claims++
	return nil, nil
}
func (s *fakeStore) Renew(context.Context, string, string, string, int) (time.Time, bool, error) {
	return time.Now(), true, nil
}
func (s *fakeStore) BindBackend(_ context.Context, _, _, _ string, record sandboxcontrol.SandboxRecord, _ sandboxcontrol.WorkloadIdentity) (bool, error) {
	s.bound, s.bindRecord = true, record
	return true, nil
}
func (s *fakeStore) Retry(_ context.Context, _, _, _ string, _ int, safe SafeError) (RetryOutcome, error) {
	s.retry = &safe
	return RetryScheduled, nil
}
func (s *fakeStore) Complete(_ context.Context, _, _, _ string, completion Completion) (bool, error) {
	s.completion = &completion
	return true, nil
}
func (*fakeStore) EnqueueExpired(context.Context, int) (int, error) { return 0, nil }
func (*fakeStore) Health(context.Context) error                     { return nil }
func (*fakeStore) Close() error                                     { return nil }

type fakeBackend struct {
	created   BackendState
	observed  BackendState
	deleting  BackendState
	finalized CleanupResult
	err       error
}

func (b *fakeBackend) Health(context.Context) error { return b.err }

func (b *fakeBackend) EnsureCreated(context.Context, WorkItem) (BackendState, error) {
	return b.created, b.err
}
func (b *fakeBackend) Observe(context.Context, WorkItem) (BackendState, error) {
	return b.observed, b.err
}
func (b *fakeBackend) BeginDelete(context.Context, WorkItem) (BackendState, error) {
	return b.deleting, b.err
}
func (b *fakeBackend) Finalize(context.Context, WorkItem, BackendState) (CleanupResult, error) {
	return b.finalized, b.err
}

func TestCreateBindsExactBackendAndCompletes(t *testing.T) {
	item, state := createFixture(t)
	store := &fakeStore{}
	controller := testController(t, store, &fakeBackend{created: state})
	if err := controller.reconcile(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if !store.bound || store.bindRecord.UID != state.Record.UID {
		t.Fatal("controller did not bind exact backend identity")
	}
	if store.completion == nil || store.completion.Status != "succeeded" ||
		store.completion.ExpectedAdmissionDigest == nil || *store.completion.ExpectedAdmissionDigest != state.Admission.Digest {
		t.Fatalf("unexpected completion: %#v", store.completion)
	}
}

func TestCreateRefusesChangedPreboundBackend(t *testing.T) {
	item, state := createFixture(t)
	item.BackendUID, item.BackendResourceVersion, item.Admission =
		pointer(state.Record.UID), pointer(state.Record.ResourceVersion), state.Admission
	item.AdmissionID = pointer(state.Admission.UID)
	state.Record.ResourceVersion = "changed"
	store := &fakeStore{}
	controller := testController(t, store, &fakeBackend{observed: state})
	if err := controller.reconcile(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if store.completion == nil || store.completion.Status != "recovery_required" || store.bound {
		t.Fatalf("changed backend was not fenced: %#v", store.completion)
	}
}

func TestCleanupRequiresBackendGrantRevocationProof(t *testing.T) {
	item, state := createFixture(t)
	item.OperationType, item.DesiredState = "delete", "deleted"
	item.BackendUID, item.BackendResourceVersion, item.Admission =
		pointer(state.Record.UID), pointer(state.Record.ResourceVersion), state.Admission
	item.AdmissionID = pointer(state.Admission.UID)
	store := &fakeStore{}
	backend := &fakeBackend{deleting: state, finalized: CleanupResult{
		CleanupComplete: true, ArtifactExportComplete: true, BackendDestroyed: true,
	}}
	controller := testController(t, store, backend)
	if err := controller.reconcile(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if store.completion == nil || store.completion.Status != "recovery_required" {
		t.Fatalf("missing revocation proof was accepted: %#v", store.completion)
	}

	store.completion = nil
	backend.finalized.GrantsRevoked = true
	if err := controller.reconcile(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if store.completion == nil || store.completion.Status != "succeeded" || !store.completion.GrantsRevoked {
		t.Fatalf("complete cleanup proof was rejected: %#v", store.completion)
	}
}

func TestUnavailableBackendSchedulesSafeRetry(t *testing.T) {
	item, _ := createFixture(t)
	store := &fakeStore{}
	controller := testController(t, store, NewUnavailableBackend())
	if err := controller.reconcile(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if store.retry == nil || store.retry.Code != "backend_unavailable" || store.retry.Message != "sandbox backend is not installed" {
		t.Fatalf("unexpected retry: %#v", store.retry)
	}
}

func TestRunFailsClosedBeforeClaimWhenBackendIsUnavailable(t *testing.T) {
	store := &fakeStore{}
	controller := testController(t, store, NewUnavailableBackend())
	if err := controller.Run(context.Background()); err == nil {
		t.Fatal("controller started without an installed backend")
	}
	if store.claims != 0 {
		t.Fatalf("unavailable backend claimed %d operations", store.claims)
	}
}

func TestInvalidPartialBackendIdentityNeverCallsBackend(t *testing.T) {
	item, _ := createFixture(t)
	item.BackendUID = pointer("backend-uid")
	store := &fakeStore{}
	backend := &fakeBackend{err: errors.New("must not be called")}
	controller := testController(t, store, backend)
	if err := controller.reconcile(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if store.completion == nil || store.completion.Status != "recovery_required" {
		t.Fatalf("partial identity was not rejected: %#v", store.completion)
	}
}

func testController(t *testing.T, store Store, backend Backend) *Controller {
	t.Helper()
	controller, err := New(store, backend, Config{WorkerID: "controller-1", Lease: 30 * time.Second,
		RenewEvery: 10 * time.Second, PollEvery: time.Millisecond, OperationTimeout: time.Second,
		IdleDelay: time.Millisecond, RetryDelay: time.Second, ExpiryEvery: time.Second, ExpiryBatch: 10})
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func createFixture(t *testing.T) (WorkItem, BackendState) {
	t.Helper()
	record := sandboxcontrol.SandboxRecord{Name: "sandbox-1", Namespace: sandboxcontrol.Namespace,
		UID: "backend-uid", ResourceVersion: "resource-version-1", WorkspaceID: "workspace-1",
		OwnerID: "owner-1", QueueName: sandboxcontrol.QueueName, State: sandboxcontrol.StateReady,
		ArtifactContractDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	identity := sandboxcontrol.WorkloadIdentity{APIVersion: sandboxcontrol.AdmissionAPIVersion,
		Namespace: sandboxcontrol.Namespace, Name: "workload.sandbox-1", UID: "admission-uid",
		ResourceVersion: "workload-version-1", ClusterQueue: "poc-queue",
		Owner: sandboxcontrol.SandboxOwnerReference{APIVersion: sandboxcontrol.APIVersion,
			Kind: sandboxcontrol.Kind, Name: record.Name, UID: record.UID, Controller: true},
		WorkspaceID: record.WorkspaceID, SandboxID: record.Name, Admitted: true,
		Condition: sandboxcontrol.AdmissionCondition{Type: "Admitted", Status: "True"}}
	receipt, err := sandboxcontrol.NewReceipt("controller-operation-123", sandboxcontrol.OperationCreate, record, nil, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err = sandboxcontrol.AttachAdmissionIdentity(receipt, identity)
	if err != nil {
		t.Fatal(err)
	}
	identity = *receipt.Admission
	item := WorkItem{OperationID: "operation-123", WorkspaceID: record.WorkspaceID,
		SandboxID: record.Name, RequestedBy: record.OwnerID, OperationType: "create",
		ExpectedSandboxVersion: 1, LeaseToken: "lease-token", Attempt: 1,
		AllocationMode: "direct", DesiredState: "ready", Architecture: "amd64",
		TemplateVersionID: "template-1", TemplateDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		VariantName: "amd64", ImageIndexDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		ImageDigest:      "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		PlacementProfile: "poc-linux-amd64-v1", QueueName: sandboxcontrol.QueueName,
		Command: []string{"true"}, Resources: Resources{CPURequest: "100m", MemoryRequest: "128Mi",
			EphemeralRequest: "1Gi", CPULimit: "1", MemoryLimit: "1Gi", EphemeralLimit: "2Gi"},
		ExpiresAt: time.Now().Add(time.Hour)}
	state := BackendState{Record: record, Admission: &identity, Exists: true, Ready: true}
	if err := validateWorkItem(item); err != nil {
		t.Fatalf("invalid work item fixture: %v", err)
	}
	if err := validateCreated(item, state); err != nil {
		t.Fatalf("invalid test fixture: %v", err)
	}
	return item, state
}

func pointer[T any](value T) *T { return &value }

package sandboxcontroller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
)

type fakeStore struct {
	bound           bool
	claims          int
	bindRecord      sandboxcontrol.SandboxRecord
	completion      *Completion
	retry           *SafeError
	renewResponses  chan renewResponse
	renewStarted    chan struct{}
	retryCalls      int
	completionCalls int
}

type renewResponse struct {
	window LeaseWindow
	ok     bool
	err    error
}

func (s *fakeStore) Claim(context.Context, string, int) (*WorkItem, error) {
	s.claims++
	return nil, nil
}
func (s *fakeStore) Renew(ctx context.Context, _ string, _ string, _ string, _ int) (LeaseWindow, bool, error) {
	if s.renewStarted != nil {
		select {
		case s.renewStarted <- struct{}{}:
		default:
		}
	}
	if s.renewResponses == nil {
		return LeaseWindow{Remaining: 5 * time.Second, Deadline: time.Now().Add(5 * time.Second)}, true, nil
	}
	select {
	case <-ctx.Done():
		return LeaseWindow{}, false, ctx.Err()
	case response := <-s.renewResponses:
		if response.window.Remaining == 0 {
			response.window.Remaining = 5 * time.Second
		}
		if response.window.Deadline.IsZero() {
			response.window.Deadline = time.Now().Add(response.window.Remaining)
		}
		return response.window, response.ok, response.err
	}
}
func (s *fakeStore) BindBackend(_ context.Context, _, _, _ string, record sandboxcontrol.SandboxRecord, _ sandboxcontrol.WorkloadIdentity) (bool, error) {
	s.bound, s.bindRecord = true, record
	return true, nil
}
func (s *fakeStore) Retry(_ context.Context, _, _, _ string, _ int, safe SafeError) (RetryOutcome, error) {
	s.retryCalls++
	s.retry = &safe
	return RetryScheduled, nil
}
func (s *fakeStore) Complete(_ context.Context, _, _, _ string, completion Completion) (bool, error) {
	s.completionCalls++
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

type blockingBackend struct {
	started   chan struct{}
	cancelled chan error
}

func newBlockingBackend() *blockingBackend {
	return &blockingBackend{started: make(chan struct{}), cancelled: make(chan error, 1)}
}

func (*blockingBackend) Health(context.Context) error { return nil }
func (b *blockingBackend) EnsureCreated(ctx context.Context, _ WorkItem) (BackendState, error) {
	close(b.started)
	<-ctx.Done()
	b.cancelled <- ctx.Err()
	return BackendState{}, ctx.Err()
}
func (b *blockingBackend) Observe(ctx context.Context, item WorkItem) (BackendState, error) {
	return b.EnsureCreated(ctx, item)
}
func (b *blockingBackend) BeginDelete(ctx context.Context, item WorkItem) (BackendState, error) {
	return b.EnsureCreated(ctx, item)
}
func (b *blockingBackend) Finalize(ctx context.Context, item WorkItem, _ BackendState) (CleanupResult, error) {
	_, err := b.EnsureCreated(ctx, item)
	return CleanupResult{}, err
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
	state.Record.ResourceVersion = "resource-version-delete"
	state.Record.Deleting = true
	state.Deleting = true
	state.CleanupFinalizerPresent = true
	state.Record.Finalizers = []string{sandboxcontrol.CleanupFinalizer}
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

func TestLeaseLossCancelsBackendAndSuppressesTerminalWrites(t *testing.T) {
	item, _ := createFixture(t)
	store := &fakeStore{renewResponses: make(chan renewResponse, 1)}
	store.renewResponses <- renewResponse{ok: false}
	backend := newBlockingBackend()
	controller := timedController(t, store, backend, 5*time.Millisecond, time.Second)
	done := make(chan error, 1)
	go func() { done <- controller.reconcile(context.Background(), item) }()
	awaitSignal(t, backend.started, "backend start")
	if err := awaitError(t, backend.cancelled, "backend cancellation"); !errors.Is(err, context.Canceled) {
		t.Fatalf("backend cancellation=%v", err)
	}
	if err := awaitError(t, done, "reconcile completion"); err != nil {
		t.Fatal(err)
	}
	if store.retryCalls != 0 || store.completionCalls != 0 {
		t.Fatalf("lease loss wrote terminal state: retries=%d completions=%d", store.retryCalls, store.completionCalls)
	}
}

func TestLeaseRenewStoreErrorCancelsBackendAndReturnsError(t *testing.T) {
	item, _ := createFixture(t)
	store := &fakeStore{renewResponses: make(chan renewResponse, 1)}
	store.renewResponses <- renewResponse{err: errors.New("renew unavailable")}
	backend := newBlockingBackend()
	controller := timedController(t, store, backend, 5*time.Millisecond, time.Second)
	done := make(chan error, 1)
	go func() { done <- controller.reconcile(context.Background(), item) }()
	awaitSignal(t, backend.started, "backend start")
	if err := awaitError(t, backend.cancelled, "backend cancellation"); !errors.Is(err, context.Canceled) {
		t.Fatalf("backend cancellation=%v", err)
	}
	if err := awaitError(t, done, "reconcile completion"); err == nil || !strings.Contains(err.Error(), "lease renewal failed") {
		t.Fatalf("renew error=%v", err)
	}
	if store.retryCalls != 0 || store.completionCalls != 0 {
		t.Fatalf("renew error wrote terminal state: retries=%d completions=%d", store.retryCalls, store.completionCalls)
	}
}

func TestClaimAndRenewRequireLeaseThroughNextHeartbeat(t *testing.T) {
	t.Run("delayed claim", func(t *testing.T) {
		item, state := createFixture(t)
		item.LeaseRemaining = defaultLeaseSafetyMargin + 5*time.Millisecond
		item.LeaseDeadline = time.Now().Add(item.LeaseRemaining)
		store := &fakeStore{}
		backend := &fakeBackend{created: state}
		controller := timedController(t, store, backend, 5*time.Millisecond, time.Second)
		if err := controller.reconcile(context.Background(), item); err != nil {
			t.Fatal(err)
		}
		if store.bound || store.completionCalls != 0 || store.retryCalls != 0 {
			t.Fatal("unsafe claimed lease performed a fenced write")
		}
	})
	t.Run("renew", func(t *testing.T) {
		item, _ := createFixture(t)
		store := &fakeStore{renewResponses: make(chan renewResponse, 1)}
		store.renewResponses <- renewResponse{window: LeaseWindow{Remaining: 25 * time.Millisecond}, ok: true}
		backend := newBlockingBackend()
		controller := timedController(t, store, backend, 5*time.Millisecond, time.Second)
		controller.leaseSafetyMargin = 20 * time.Millisecond
		done := make(chan error, 1)
		go func() { done <- controller.reconcile(context.Background(), item) }()
		awaitSignal(t, backend.started, "backend start")
		if err := awaitError(t, backend.cancelled, "backend cancellation"); !errors.Is(err, context.Canceled) {
			t.Fatalf("backend cancellation=%v", err)
		}
		if err := awaitError(t, done, "reconcile completion"); err != nil {
			t.Fatal(err)
		}
		if store.completionCalls != 0 || store.retryCalls != 0 {
			t.Fatal("unsafe renewed lease performed a fenced write")
		}
	})
}

func TestStalledRenewalCancelsBackendBeforeLeaseIsReclaimable(t *testing.T) {
	item, _ := createFixture(t)
	item.LeaseRemaining = 80 * time.Millisecond
	item.LeaseDeadline = time.Now().Add(item.LeaseRemaining)
	store := &fakeStore{renewResponses: make(chan renewResponse), renewStarted: make(chan struct{}, 1)}
	backend := newBlockingBackend()
	controller := timedController(t, store, backend, 5*time.Millisecond, time.Second)
	controller.leaseSafetyMargin = 20 * time.Millisecond
	done := make(chan error, 1)
	reclaimable := time.NewTimer(item.LeaseRemaining)
	defer reclaimable.Stop()
	go func() { done <- controller.reconcile(context.Background(), item) }()
	awaitSignal(t, backend.started, "backend start")
	awaitSignal(t, store.renewStarted, "renewal start")
	select {
	case err := <-backend.cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("backend cancellation=%v", err)
		}
	case <-reclaimable.C:
		t.Fatal("backend remained active until the database lease became reclaimable")
	}
	if err := awaitError(t, done, "reconcile completion"); err != nil {
		t.Fatal(err)
	}
	if store.retryCalls != 0 || store.completionCalls != 0 {
		t.Fatalf("stalled renewal wrote terminal state: retries=%d completions=%d", store.retryCalls, store.completionCalls)
	}
}

func TestRenewedDeadlineReplacesClaimDeadline(t *testing.T) {
	item, _ := createFixture(t)
	item.LeaseRemaining = 80 * time.Millisecond
	item.LeaseDeadline = time.Now().Add(item.LeaseRemaining)
	store := &fakeStore{renewResponses: make(chan renewResponse, 1), renewStarted: make(chan struct{}, 2)}
	store.renewResponses <- renewResponse{window: LeaseWindow{Remaining: 150 * time.Millisecond}, ok: true}
	backend := newBlockingBackend()
	controller := timedController(t, store, backend, 20*time.Millisecond, time.Second)
	controller.leaseSafetyMargin = 20 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- controller.reconcile(context.Background(), item) }()
	awaitSignal(t, backend.started, "backend start")
	awaitSignal(t, store.renewStarted, "first renewal")
	awaitSignal(t, store.renewStarted, "second renewal")
	if err := awaitError(t, backend.cancelled, "renewed lease watchdog cancellation"); !errors.Is(err, context.Canceled) {
		t.Fatalf("backend cancellation=%v", err)
	}
	if err := awaitError(t, done, "reconcile completion"); err != nil {
		t.Fatal(err)
	}
	if store.retryCalls != 0 || store.completionCalls != 0 {
		t.Fatalf("renewed deadline wrote terminal state: retries=%d completions=%d", store.retryCalls, store.completionCalls)
	}
}

func TestParentShutdownCancelsBackendWithoutTerminalWrite(t *testing.T) {
	item, _ := createFixture(t)
	store, backend := &fakeStore{}, newBlockingBackend()
	controller := timedController(t, store, backend, 100*time.Millisecond, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- controller.reconcile(ctx, item) }()
	awaitSignal(t, backend.started, "backend start")
	cancel()
	if err := awaitError(t, backend.cancelled, "backend cancellation"); !errors.Is(err, context.Canceled) {
		t.Fatalf("backend cancellation=%v", err)
	}
	if err := awaitError(t, done, "reconcile completion"); err != nil {
		t.Fatal(err)
	}
	if store.retryCalls != 0 || store.completionCalls != 0 {
		t.Fatalf("shutdown wrote terminal state: retries=%d completions=%d", store.retryCalls, store.completionCalls)
	}
}

func TestOperationTimeoutCancelsBackendAndSchedulesRetry(t *testing.T) {
	item, _ := createFixture(t)
	store, backend := &fakeStore{}, newBlockingBackend()
	controller := timedController(t, store, backend, 100*time.Millisecond, 20*time.Millisecond)
	if err := controller.reconcile(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if err := awaitError(t, backend.cancelled, "backend timeout"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("backend timeout=%v", err)
	}
	if store.retryCalls != 1 || store.completionCalls != 0 {
		t.Fatalf("timeout terminal writes: retries=%d completions=%d", store.retryCalls, store.completionCalls)
	}
}

func TestOperationTimeoutWhileRenewIsInFlightStillSchedulesRetry(t *testing.T) {
	item, _ := createFixture(t)
	store := &fakeStore{renewResponses: make(chan renewResponse)}
	backend := newBlockingBackend()
	controller := timedController(t, store, backend, 5*time.Millisecond, 20*time.Millisecond)
	if err := controller.reconcile(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if err := awaitError(t, backend.cancelled, "backend timeout"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("backend timeout=%v", err)
	}
	if store.retryCalls != 1 || store.completionCalls != 0 {
		t.Fatalf("timeout terminal writes: retries=%d completions=%d", store.retryCalls, store.completionCalls)
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

func timedController(t *testing.T, store Store, backend Backend, renewEvery, operationTimeout time.Duration) *Controller {
	t.Helper()
	controller, err := New(store, backend, Config{WorkerID: "controller-1", Lease: 5 * time.Second,
		RenewEvery: renewEvery, PollEvery: time.Millisecond, OperationTimeout: operationTimeout,
		IdleDelay: time.Millisecond, RetryDelay: time.Second, ExpiryEvery: time.Second, ExpiryBatch: 10})
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func awaitSignal(t *testing.T, channel <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func awaitError(t *testing.T, channel <-chan error, label string) error {
	t.Helper()
	select {
	case err := <-channel:
		return err
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", label)
		return nil
	}
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
		VariantName: "amd64", ImageIndexDigest: "registry.invalid/poc@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		ImageDigest:      "registry.invalid/poc@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		PlacementProfile: "poc-linux-amd64-v1", QueueName: sandboxcontrol.QueueName,
		Command: []string{"true"}, Resources: Resources{CPURequest: "100m", MemoryRequest: "128Mi",
			EphemeralRequest: "1Gi", CPULimit: "1", MemoryLimit: "1Gi", EphemeralLimit: "2Gi"},
		LeaseExpiresAt: time.Now().Add(time.Minute), LeaseRemaining: time.Minute,
		LeaseDeadline: time.Now().Add(time.Minute), ExpiresAt: time.Now().Add(time.Hour)}
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

package sandboxcontroller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxcontrol"
	"github.com/blazncloud/blazn/internal/sandboxio"
)

type fakeStore struct {
	bound           bool
	claims          int
	bindObservation sandboxcontrol.AdmissionObservation
	sourceReceipt   *sandboxio.SourceMaterializationReceipt
	completion      *Completion
	retry           *SafeError
	renewResponses  chan renewResponse
	renewStarted    chan struct{}
	retryCalls      int
	completionCalls int
	artifactRecords int
	artifactPhases  int
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
func (s *fakeStore) BindBackend(_ context.Context, _, _, _ string, observation sandboxcontrol.AdmissionObservation) (bool, error) {
	s.bound, s.bindObservation = true, observation
	return true, nil
}
func (s *fakeStore) RecordSources(_ context.Context, _, _, _ string, _ sandboxcontrol.AdmissionObservation, receipt sandboxio.SourceMaterializationReceipt) (bool, error) {
	s.sourceReceipt = &receipt
	return true, nil
}
func (s *fakeStore) RecordArtifact(_ context.Context, _, _, _ string, _ sandboxcontrol.AdmissionObservation, artifact PersistedArtifact) (PersistedArtifact, bool, error) {
	s.artifactRecords++
	artifact.ID = "80000000-0000-4000-8000-000000000001"
	artifact.ExportedAt = "2026-08-24T12:00:00Z"
	return artifact, true, nil
}
func (s *fakeStore) CompleteArtifactExport(_ context.Context, _, _, _ string, _ sandboxcontrol.AdmissionObservation, warnings []string) (bool, error) {
	s.artifactPhases++
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
	created                                      BackendState
	observed                                     BackendState
	observedStates                               []BackendState
	deleting                                     BackendState
	finalized                                    CleanupResult
	err                                          error
	calls                                        int
	prepared, materialized, restricted, released int
	artifactExports                              int
	artifactResult                               ArtifactExportResult
	order                                        []string
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
func (b *blockingBackend) Observe(ctx context.Context, item WorkItem, _ *sandboxcontrol.AdmissionObservation) (BackendState, error) {
	return b.EnsureCreated(ctx, item)
}
func (b *blockingBackend) BeginDelete(ctx context.Context, item WorkItem, _ *sandboxcontrol.AdmissionObservation) (BackendState, error) {
	return b.EnsureCreated(ctx, item)
}
func (b *blockingBackend) Finalize(ctx context.Context, item WorkItem, _ BackendState, _ *sandboxcontrol.AdmissionObservation) (CleanupResult, error) {
	_, err := b.EnsureCreated(ctx, item)
	return CleanupResult{}, err
}

func (b *fakeBackend) Health(context.Context) error { return b.err }

func (b *fakeBackend) EnsureCreated(context.Context, WorkItem) (BackendState, error) {
	b.calls++
	b.order = append(b.order, "create")
	return b.created, b.err
}
func (b *fakeBackend) Observe(context.Context, WorkItem, *sandboxcontrol.AdmissionObservation) (BackendState, error) {
	b.calls++
	b.order = append(b.order, "observe")
	if len(b.observedStates) != 0 {
		state := b.observedStates[0]
		b.observedStates = b.observedStates[1:]
		return state, b.err
	}
	return b.observed, b.err
}
func (b *fakeBackend) BeginDelete(context.Context, WorkItem, *sandboxcontrol.AdmissionObservation) (BackendState, error) {
	b.calls++
	b.order = append(b.order, "delete")
	return b.deleting, b.err
}
func (b *fakeBackend) Finalize(context.Context, WorkItem, BackendState, *sandboxcontrol.AdmissionObservation) (CleanupResult, error) {
	b.calls++
	b.order = append(b.order, "finalize")
	return b.finalized, b.err
}
func (b *fakeBackend) PrepareSourceBootstrap(context.Context, WorkItem, sandboxcontrol.AdmissionObservation) error {
	b.prepared++
	return b.err
}
func (b *fakeBackend) MaterializeSources(_ context.Context, item WorkItem, _ sandboxcontrol.AdmissionObservation) (sandboxio.SourceMaterializationReceipt, error) {
	b.materialized++
	manifest := sourceManifest(item.Sources)
	sources := make([]sandboxio.SourceMaterialization, len(item.Sources))
	for index, source := range item.Sources {
		sources[index] = sandboxio.SourceMaterialization{Name: source.Name, URL: source.URL, Destination: source.Destination,
			Commit: source.Commit, Tree: source.Commit, ContentDigest: "sha256:" + strings.Repeat("e", 64), Writable: source.Writable}
	}
	receipt, err := sandboxio.NewSourceMaterializationReceipt(manifest, sources)
	return receipt, errors.Join(b.err, err)
}
func (b *fakeBackend) RestrictSourceRuntime(context.Context, WorkItem, sandboxcontrol.AdmissionObservation, sandboxio.SourceMaterializationReceipt) error {
	b.restricted++
	return b.err
}
func (b *fakeBackend) ReleaseSources(context.Context, WorkItem, sandboxcontrol.AdmissionObservation, sandboxio.SourceMaterializationReceipt) error {
	b.released++
	return b.err
}
func (b *fakeBackend) ExportArtifacts(context.Context, WorkItem, sandboxcontrol.AdmissionObservation) (ArtifactExportResult, error) {
	b.artifactExports++
	b.order = append(b.order, "export")
	return b.artifactResult, b.err
}

func TestCreateBindsExactBackendAndCompletes(t *testing.T) {
	item, state := createFixture(t)
	store := &fakeStore{}
	controller := testController(t, store, &fakeBackend{created: state})
	if err := controller.reconcile(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if !store.bound || store.bindObservation.Sandbox.UID != state.Record.UID {
		t.Fatal("controller did not bind exact backend identity")
	}
	if store.completion == nil || store.completion.Status != "succeeded" ||
		store.completion.ExpectedWorkloadDigest == nil || *store.completion.ExpectedWorkloadDigest != state.AdmissionObservation.Workload.Digest ||
		store.completion.ExpectedObservationDigest == nil || *store.completion.ExpectedObservationDigest != state.AdmissionObservation.Digest {
		t.Fatalf("unexpected completion: %#v", store.completion)
	}
}

func TestCreatePersistsSourcesBeforeRuntimeRestrictionAndRelease(t *testing.T) {
	item, ready := createFixture(t)
	item.Sources = []Source{{Name: "repo", URL: "https://example.test/owner/repo.git", Destination: "/workspace/src/repo", Commit: strings.Repeat("a", 40)}}
	pending := ready
	pending.Ready = false
	pending.Record.State = sandboxcontrol.StatePending
	created := pending
	created.AdmissionObservation = nil
	store := &fakeStore{}
	backend := &fakeBackend{created: created, observedStates: []BackendState{pending, ready}}
	if err := testController(t, store, backend).reconcile(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if !store.bound || store.sourceReceipt == nil || backend.prepared != 1 || backend.materialized != 1 || backend.restricted != 1 || backend.released != 1 {
		t.Fatalf("source sequence store=%#v backend=%#v", store.sourceReceipt, backend)
	}
	if store.completion == nil || store.completion.Status != "succeeded" {
		t.Fatalf("source create did not complete: %#v", store.completion)
	}
}

func TestCreateRestartAdoptsPersistedSourceReceiptWithoutReleasingReadyPod(t *testing.T) {
	item, ready := createFixture(t)
	item.Sources = []Source{{Name: "repo", URL: "https://example.test/owner/repo.git", Destination: "/workspace/src/repo", Commit: strings.Repeat("a", 40)}}
	bindWorkItem(&item, ready)
	manifest := sourceManifest(item.Sources)
	receipt, err := sandboxio.NewSourceMaterializationReceipt(manifest, []sandboxio.SourceMaterialization{{Name: "repo", URL: item.Sources[0].URL,
		Destination: item.Sources[0].Destination, Commit: item.Sources[0].Commit, Tree: item.Sources[0].Commit,
		ContentDigest: "sha256:" + strings.Repeat("e", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	item.SourceMaterialization = &receipt
	store, backend := &fakeStore{}, &fakeBackend{observed: ready}
	if err := testController(t, store, backend).reconcile(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if backend.prepared != 0 || backend.materialized != 0 || backend.restricted != 1 || backend.released != 0 || store.sourceReceipt != nil || store.completion == nil {
		t.Fatalf("restart did not adopt persisted source receipt: backend=%#v store=%#v", backend, store)
	}
}

func TestCreateRefusesChangedPreboundBackend(t *testing.T) {
	item, state := createFixture(t)
	bindWorkItem(&item, state)
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

func TestLegacyWorkloadOnlyClaimRecoversBeforeBackendMutation(t *testing.T) {
	item, state := createFixture(t)
	item.OperationType, item.DesiredState = "delete", "deleted"
	item.BackendUID = pointer(state.Record.UID)
	item.BackendResourceVersion = pointer(state.Record.ResourceVersion)
	item.AdmissionID = pointer(state.AdmissionObservation.Workload.UID)
	item.PersistedWorkloadDigest = pointer(state.AdmissionObservation.Workload.Digest)
	item.AdmissionObservation = nil
	store, backend := &fakeStore{}, &fakeBackend{}
	if err := testController(t, store, backend).reconcile(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if backend.calls != 0 || store.completion == nil || store.completion.Status != "recovery_required" ||
		store.completion.ExpectedWorkloadDigest == nil || store.completion.ExpectedObservationDigest != nil {
		t.Fatalf("legacy observation path mutated backend or escaped recovery: calls=%d completion=%#v", backend.calls, store.completion)
	}
}

func TestCleanupRequiresBackendGrantRevocationProof(t *testing.T) {
	item, state := createFixture(t)
	item.OperationType, item.DesiredState = "delete", "deleted"
	bindWorkItem(&item, state)
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

func TestCleanupExportsAndPersistsArtifactsBeforeBackendDelete(t *testing.T) {
	item, state := createFixture(t)
	artifactItem, observation := artifactRuntimeFixture(t)
	item.WorkspaceID, item.SandboxID = artifactItem.WorkspaceID, artifactItem.SandboxID
	item.OperationType, item.DesiredState = "stop", "stopped"
	item.BackendUID, item.BackendResourceVersion = artifactItem.BackendUID, artifactItem.BackendResourceVersion
	item.AdmissionID = pointer(observation.Workload.UID)
	item.PersistedWorkloadDigest = pointer(observation.Workload.Digest)
	item.AdmissionObservation = &observation
	item.Artifacts = []Artifact{{Name: "result", Path: "/workspace/artifacts/result", MediaType: "text/plain", Required: true}}
	state.Record.Name, state.Record.WorkspaceID = item.SandboxID, item.WorkspaceID
	state.Record.UID, state.Record.ResourceVersion = observation.Sandbox.UID, observation.Sandbox.ResourceVersion
	state.Record.ResourceVersion = "resource-version-delete"
	state.Record.State, state.Record.Deleting = sandboxcontrol.StateStopping, true
	state.Record.Finalizers = []string{sandboxcontrol.CleanupFinalizer}
	state.Record.Artifacts = []sandboxcontrol.ArtifactExport{{Name: "result", Path: item.Artifacts[0].Path, MediaType: "text/plain", Required: true}}
	_, state.Record.ArtifactContractDigest, _ = sandboxcontrol.CanonicalArtifactContract(state.Record.Artifacts)
	state.AdmissionObservation, state.Exists, state.Ready, state.Deleting, state.CleanupFinalizerPresent = &observation, true, false, true, true
	key, _ := ArtifactObjectKey(item.WorkspaceID, item.SandboxID, "result")
	exported := PersistedArtifact{Name: "result", Path: item.Artifacts[0].Path, MediaType: "text/plain",
		Digest: "sha256:" + strings.Repeat("d", 64), Size: 6, ObjectKey: key}
	store := &fakeStore{}
	backend := &fakeBackend{deleting: state, artifactResult: ArtifactExportResult{Artifacts: []PersistedArtifact{exported}, WarningCodes: []string{}},
		finalized: CleanupResult{ArtifactIDs: []string{"80000000-0000-4000-8000-000000000001"}, WarningCodes: []string{},
			CleanupComplete: true, ArtifactExportComplete: true, GrantsRevoked: true, BackendDestroyed: true}}
	if err := testController(t, store, backend).reconcile(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(backend.order, []string{"export", "delete", "finalize"}) || store.artifactRecords != 1 || store.artifactPhases != 1 ||
		store.completion == nil || !reflect.DeepEqual(store.completion.ArtifactIDs, []string{"80000000-0000-4000-8000-000000000001"}) {
		t.Fatalf("order=%v records=%d phases=%d completion=%#v", backend.order, store.artifactRecords, store.artifactPhases, store.completion)
	}
}

func TestCleanupRestartAdoptsCompletedArtifactPhase(t *testing.T) {
	item, state := createFixture(t)
	item.OperationType, item.DesiredState = "delete", "deleted"
	bindWorkItem(&item, state)
	item.ArtifactExportComplete = true
	item.ArtifactWarningCodes = []string{"optional_artifact_missing_logs"}
	item.Artifacts = []Artifact{{Name: "logs", Path: "/workspace/artifacts/logs", MediaType: "text/plain", Required: false}}
	state.Record.Artifacts = []sandboxcontrol.ArtifactExport{{Name: "logs", Path: "/workspace/artifacts/logs", MediaType: "text/plain", Required: false}}
	_, state.Record.ArtifactContractDigest, _ = sandboxcontrol.CanonicalArtifactContract(state.Record.Artifacts)
	state.Record.ResourceVersion, state.Record.Deleting, state.Deleting = "resource-version-delete", true, true
	state.Record.Finalizers = []string{sandboxcontrol.CleanupFinalizer}
	state.CleanupFinalizerPresent = true
	store := &fakeStore{}
	backend := &fakeBackend{deleting: state, finalized: CleanupResult{CleanupComplete: true,
		ArtifactExportComplete: true, GrantsRevoked: true, BackendDestroyed: true}}
	if err := testController(t, store, backend).reconcile(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if backend.artifactExports != 0 || store.artifactPhases != 0 || store.completion == nil ||
		!reflect.DeepEqual(store.completion.WarningCodes, item.ArtifactWarningCodes) {
		t.Fatalf("restart repeated export or lost receipt: exports=%d phases=%d completion=%#v", backend.artifactExports, store.artifactPhases, store.completion)
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
	if store.completion.Error == nil || store.completion.Error.Message != "controller work item is invalid: persisted backend identity is incomplete" {
		t.Fatalf("invalid work-item category was not retained safely: %#v", store.completion.Error)
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

func TestValidateWorkItemRejectsNoncanonicalAndDuplicateSources(t *testing.T) {
	item, _ := createFixture(t)
	valid := Source{Name: "source", URL: "https://github.com/blazncloud/blazn.git", Destination: "/workspace/src/source", Commit: strings.Repeat("a", 40)}
	item.Sources = []Source{valid}
	if err := validateWorkItem(item); err != nil {
		t.Fatalf("canonical source rejected: %v", err)
	}
	for label, mutate := range map[string]func(*Source){
		"userinfo":           func(value *Source) { value.URL = "https://user@example.test/repo.git" },
		"query":              func(value *Source) { value.URL += "?token=forbidden" },
		"fragment":           func(value *Source) { value.URL += "#main" },
		"uppercase host":     func(value *Source) { value.URL = "https://GitHub.com/blazncloud/blazn.git" },
		"empty host label":   func(value *Source) { value.URL = "https://example..test/repo.git" },
		"invalid host label": func(value *Source) { value.URL = "https://-example.test/repo.git" },
		"noncanonical ipv4":  func(value *Source) { value.URL = "https://010.0.0.1/repo.git" },
		"named port":         func(value *Source) { value.URL = "https://example.test:https/repo.git" },
		"unapproved port":    func(value *Source) { value.URL = "https://example.test:8443/repo.git" },
		"encoded path":       func(value *Source) { value.URL = "https://example.test/repo%2egit" },
		"path traversal":     func(value *Source) { value.Destination = "/workspace/src/../escape" },
		"wrong root":         func(value *Source) { value.Destination = "/workspace/other/source" },
		"mutable commit":     func(value *Source) { value.Commit = "main" },
		"noncanonical name":  func(value *Source) { value.Name = "Source" },
	} {
		t.Run(label, func(t *testing.T) {
			candidate := item
			source := valid
			mutate(&source)
			candidate.Sources = []Source{source}
			if err := validateWorkItem(candidate); err == nil {
				t.Fatalf("unsafe source accepted: %#v", source)
			}
		})
	}
	for label, source := range map[string]Source{
		"duplicate name":        {Name: valid.Name, URL: "https://example.test/other.git", Destination: "/workspace/src/other", Commit: strings.Repeat("b", 40)},
		"duplicate destination": {Name: "other", URL: "https://example.test/other.git", Destination: valid.Destination, Commit: strings.Repeat("b", 40)},
		"nested destination":    {Name: "other", URL: "https://example.test/other.git", Destination: valid.Destination + "/vendor", Commit: strings.Repeat("b", 40)},
	} {
		t.Run(label, func(t *testing.T) {
			candidate := item
			candidate.Sources = []Source{valid, source}
			if err := validateWorkItem(candidate); err == nil {
				t.Fatal("duplicate source identity accepted")
			}
		})
	}
	reversed := item
	reversed.Sources = []Source{{Name: "child", URL: "https://example.test/child.git", Destination: valid.Destination + "/vendor", Commit: strings.Repeat("b", 40)}, valid}
	if err := validateWorkItem(reversed); err == nil {
		t.Fatal("reverse-ordered nested destination was accepted")
	}
	intervening := item
	intervening.Sources = []Source{
		{Name: "parent", URL: "https://example.test/parent.git", Destination: "/workspace/src/a", Commit: strings.Repeat("a", 40)},
		{Name: "sibling", URL: "https://example.test/sibling.git", Destination: "/workspace/src/a-foo", Commit: strings.Repeat("b", 40)},
		{Name: "child", URL: "https://example.test/child.git", Destination: "/workspace/src/a/x", Commit: strings.Repeat("c", 40)},
	}
	if err := validateWorkItem(intervening); err == nil {
		t.Fatal("lexically intervening nested destination was accepted")
	}
}

func createFixture(t *testing.T) (WorkItem, BackendState) {
	t.Helper()
	record := sandboxcontrol.SandboxRecord{Name: "30000000-0000-4000-8000-000000000001", Namespace: sandboxcontrol.Namespace,
		UID: "backend-uid", ResourceVersion: "resource-version-1", WorkspaceID: "40000000-0000-4000-8000-000000000001",
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
		VariantName: "amd64", ImageIndexDigest: "registry.example.test/blazn/sandbox@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		ImageDigest:      "registry.example.test/blazn/sandbox@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		PlacementProfile: "poc-linux-amd64-v1", QueueName: sandboxcontrol.QueueName,
		Command: []string{"true"}, Resources: Resources{CPURequest: "100m", MemoryRequest: "128Mi",
			EphemeralRequest: "1Gi", CPULimit: "1", MemoryLimit: "1Gi", EphemeralLimit: "2Gi"},
		LeaseExpiresAt: time.Now().Add(time.Minute), LeaseRemaining: time.Minute,
		LeaseDeadline: time.Now().Add(time.Minute), ExpiresAt: time.Now().Add(time.Hour)}
	observation := sandboxcontrol.AdmissionObservation{
		Sandbox: sandboxcontrol.ObjectIdentity{APIVersion: sandboxcontrol.APIVersion, Kind: sandboxcontrol.Kind,
			Namespace: sandboxcontrol.Namespace, Name: record.Name, UID: record.UID, ResourceVersion: record.ResourceVersion},
		Pod: sandboxcontrol.ObjectIdentity{APIVersion: "v1", Kind: "Pod", Namespace: sandboxcontrol.Namespace,
			Name: "pod.sandbox-1", UID: "pod-uid", ResourceVersion: "pod-version-1"},
		Workload: identity,
	}
	observation.Digest = sandboxcontrol.AdmissionObservationDigest(observation)
	state := BackendState{Record: record, AdmissionObservation: &observation, Exists: true, Ready: true}
	if err := validateWorkItem(item); err != nil {
		t.Fatalf("invalid work item fixture: %v", err)
	}
	if err := validateCreated(item, state); err != nil {
		t.Fatalf("invalid test fixture: %v", err)
	}
	return item, state
}

func bindWorkItem(item *WorkItem, state BackendState) {
	item.BackendUID = pointer(state.Record.UID)
	item.BackendResourceVersion = pointer(state.Record.ResourceVersion)
	item.AdmissionID = pointer(state.AdmissionObservation.Workload.UID)
	item.PersistedWorkloadDigest = pointer(state.AdmissionObservation.Workload.Digest)
	item.AdmissionObservation = state.AdmissionObservation
}

func TestValidateWorkItemRejectsMalformedImageReferences(t *testing.T) {
	valid, _ := createFixture(t)
	tests := []struct {
		name      string
		reference string
	}{
		{name: "tag", reference: "registry.example.test/blazn/sandbox:latest"},
		{name: "digest only", reference: "sha256:" + strings.Repeat("a", 64)},
		{name: "uppercase", reference: "registry.example.test/Blazn/sandbox@sha256:" + strings.Repeat("a", 64)},
		{name: "malformed digest", reference: "registry.example.test/blazn/sandbox@sha256:abc"},
		{name: "credentials", reference: "user:password@registry.example.test/blazn/sandbox@sha256:" + strings.Repeat("a", 64)},
		{name: "parent traversal", reference: "registry.example.test/blazn/../sandbox@sha256:" + strings.Repeat("a", 64)},
		{name: "empty segment", reference: "registry.example.test/blazn//sandbox@sha256:" + strings.Repeat("a", 64)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := valid
			item.ImageIndexDigest = test.reference
			if err := validateWorkItem(item); err == nil {
				t.Fatalf("malformed image index reference %q was accepted", test.reference)
			}
			item = valid
			item.ImageDigest = test.reference
			if err := validateWorkItem(item); err == nil {
				t.Fatalf("malformed image reference %q was accepted", test.reference)
			}
		})
	}
}

func pointer[T any](value T) *T { return &value }

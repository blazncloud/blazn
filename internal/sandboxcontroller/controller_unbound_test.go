package sandboxcontroller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/sandboxio"
)

type destroyingBackend struct {
	*fakeBackend
	destroyed  int
	destroyErr error
}

func (b *destroyingBackend) DestroyUnboundCreate(context.Context, WorkItem) error {
	b.destroyed++
	return b.destroyErr
}

func unboundCreate(t *testing.T, attempt int) WorkItem {
	t.Helper()
	item, _ := createFixture(t)
	item.BackendUID, item.BackendResourceVersion, item.AdmissionID = nil, nil, nil
	item.PersistedWorkloadDigest, item.AdmissionObservation = nil, nil
	item.Attempt = attempt
	return item
}

func reconcileUnbound(t *testing.T, item WorkItem, failure error, destroyErr error) (*fakeStore, *destroyingBackend) {
	t.Helper()
	store := &fakeStore{}
	backend := &destroyingBackend{fakeBackend: &fakeBackend{err: failure}, destroyErr: destroyErr}
	// The fake store grants a 5s lease, so renew well inside it: the destroy
	// step refuses a lease that would not survive one renewal.
	if err := timedController(t, store, backend, 100*time.Millisecond, time.Second).reconcile(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	return store, backend
}

func assertDestroyedCreate(t *testing.T, store *fakeStore, backend *destroyingBackend, code string) {
	t.Helper()
	completion := store.completion
	if backend.destroyed != 1 || store.retryCalls != 0 || completion == nil || completion.Status != "failed" ||
		!completion.CleanupComplete || !completion.GrantsRevoked || !completion.BackendDestroyed || completion.ArtifactExportComplete ||
		completion.ExpectedBackendUID != nil || completion.Error == nil || completion.Error.Code != code {
		t.Fatalf("destroyed=%d retries=%d completion=%#v", backend.destroyed, store.retryCalls, completion)
	}
}

func TestNonRetryableUnboundCreateFailureDestroysItsBackend(t *testing.T) {
	store, backend := reconcileUnbound(t, unboundCreate(t, 1), &Failure{Code: "backend_request_rejected", SafeMessage: "rejected"}, nil)
	assertDestroyedCreate(t, store, backend, "backend_request_rejected")
}

func TestAmbiguousUnboundCreateFailureDestroysItsBackendInsteadOfRecovery(t *testing.T) {
	store, backend := reconcileUnbound(t, unboundCreate(t, 1), &Failure{Code: "backend_identity_mismatch", SafeMessage: "changed", Ambiguous: true}, nil)
	assertDestroyedCreate(t, store, backend, "backend_identity_mismatch")
}

func TestRetryableUnboundCreateFailureKeepsItsBackendForTheNextAttempt(t *testing.T) {
	store, backend := reconcileUnbound(t, unboundCreate(t, finalCreateAttempt-1), errors.New("pod never scheduled"), nil)
	if backend.destroyed != 0 || store.retryCalls != 1 || store.completionCalls != 0 {
		t.Fatalf("destroyed=%d retries=%d completions=%d", backend.destroyed, store.retryCalls, store.completionCalls)
	}
}

func TestFinalAttemptUnboundCreateFailureDestroysInsteadOfExhaustingRetries(t *testing.T) {
	store, backend := reconcileUnbound(t, unboundCreate(t, finalCreateAttempt), errors.New("pod never scheduled"), nil)
	assertDestroyedCreate(t, store, backend, "backend_failure")
}

func TestUnprovenUnboundDestroyRecordsTheOriginalFailure(t *testing.T) {
	store, backend := reconcileUnbound(t, unboundCreate(t, 1), &Failure{Code: "backend_request_rejected", SafeMessage: "rejected"}, errors.New("pod remains"))
	if backend.destroyed != 1 || store.completion == nil || store.completion.Status != "failed" || store.completion.BackendDestroyed || store.completion.CleanupComplete {
		t.Fatalf("destroyed=%d completion=%#v", backend.destroyed, store.completion)
	}
	store, backend = reconcileUnbound(t, unboundCreate(t, 1), &Failure{Code: "backend_identity_mismatch", SafeMessage: "changed", Ambiguous: true}, errors.New("pod remains"))
	if store.completion == nil || store.completion.Status != "recovery_required" || store.completion.BackendDestroyed {
		t.Fatalf("ambiguous unproven completion=%#v", store.completion)
	}
}

func TestBoundOrSourcedCreateFailureNeverDestroysUnbound(t *testing.T) {
	bound, state := createFixture(t)
	bindWorkItem(&bound, state)
	_, backend := reconcileUnbound(t, bound, &Failure{Code: "backend_request_rejected", SafeMessage: "rejected"}, nil)
	if backend.destroyed != 0 {
		t.Fatal("a bound create destroyed its backend through the unbound path")
	}
	if destroysUnboundCreate(WorkItem{OperationType: "create", SourceMaterialization: &sandboxio.SourceMaterializationReceipt{}}, &Failure{Code: "x"}) {
		t.Fatal("a create with recorded sources took the unbound path")
	}
	if destroysUnboundCreate(WorkItem{OperationType: "stop"}, &Failure{Code: "x"}) {
		t.Fatal("a stop took the unbound create path")
	}
}

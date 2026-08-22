//go:build darwin || linux

package state

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestStoreWritesChecksummedSingleLinkRecords(t *testing.T) {
	store := testStore(t)
	journal := testJournal()
	receipt := writeActiveRecords(t, store, journal)

	readJournal, err := readJournal(store.paths.Journal, store.uid)
	if err != nil {
		t.Fatal(err)
	}
	readReceipt, err := readReceipt(store.paths.Receipt, store.uid)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateBinding(readJournal, readReceipt); err != nil {
		t.Fatal(err)
	}
	if readJournal.Checksum == "" || readReceipt.Checksum == "" || receipt.Checksum == "" {
		t.Fatal("records were not checksummed")
	}
	for _, path := range []string{store.paths.Journal, store.paths.Receipt, store.paths.Lock} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode is %o", path, info.Mode().Perm())
		}
	}
	receiptBytes, err := os.ReadFile(store.paths.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(receiptBytes, []byte("prior-value")) {
		t.Fatal("receipt leaked a prior environment value")
	}
}

func TestAccountPathsIgnoreHOMEAndXDG(t *testing.T) {
	original := lookupAccount
	t.Cleanup(func() { lookupAccount = original })
	lookupAccount = func(string) (string, error) { return "/account/home", nil }
	t.Setenv("HOME", "/attacker/home")
	t.Setenv("XDG_DATA_HOME", "/attacker/xdg")

	linux, err := AccountPaths("linux", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if linux.Root != "/account/home/.local/share/blazn/proxy" {
		t.Fatalf("unexpected Linux path %q", linux.Root)
	}
	darwin, err := AccountPaths("darwin", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if darwin.Root != "/account/home/Library/Application Support/Blazn/proxy" {
		t.Fatalf("unexpected Darwin path %q", darwin.Root)
	}
}

func TestJournalRequiresExactlyOneOfEachEnvironmentName(t *testing.T) {
	journal := testJournal()
	journal.Environment[4].Name = journal.Environment[0].Name
	if err := journal.Validate(); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("duplicate environment error = %v", err)
	}
	journal = testJournal()
	journal.Nonce = "contains spaces and is much longer than thirty two bytes"
	if err := journal.Validate(); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("invalid nonce error = %v", err)
	}
	journal = testJournal()
	journal.Listener.Address = "0.0.0.0:8123"
	if err := journal.Validate(); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("non-loopback listener error = %v", err)
	}
}

func TestLifecycleReservationSerializesConcurrentCallers(t *testing.T) {
	store := testStore(t)
	start := make(chan struct{})
	var successes atomic.Int32
	var unexpected atomic.Int32
	var wait sync.WaitGroup
	for i := 0; i < 24; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			nonce := nonceFor(byte('A' + index%26))
			_, err := store.Reserve(context.Background(), nonce, time.Minute)
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrLifecycleConflict) {
				unexpected.Add(1)
			}
		}(i)
	}
	close(start)
	wait.Wait()
	if successes.Load() != 1 || unexpected.Load() != 0 {
		t.Fatalf("successes=%d unexpected=%d", successes.Load(), unexpected.Load())
	}
}

func TestReservationMustMatchAndIsConsumedAfterSuccess(t *testing.T) {
	store := testStore(t)
	reservation, err := store.Reserve(context.Background(), nonceFor('r'), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	changed := reservation
	changed.Nonce = nonceFor('x')
	if err := store.WithReservation(context.Background(), changed, func(*ActivationTransaction) error { return nil }); !errors.Is(err, ErrLifecycleConflict) {
		t.Fatalf("mismatched reservation error = %v", err)
	}
	callbackErr := errors.New("publication failed")
	if err := store.WithReservation(context.Background(), reservation, func(*ActivationTransaction) error { return callbackErr }); !errors.Is(err, callbackErr) {
		t.Fatalf("callback error = %v", err)
	}
	if err := store.WithReservation(context.Background(), reservation, func(*ActivationTransaction) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.paths.Reservation); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reservation was not consumed: %v", err)
	}
}

func TestEscapedTransactionCannotMutateAfterFenceIsConsumed(t *testing.T) {
	store := testStore(t)
	reservation, err := store.Reserve(context.Background(), nonceFor('e'), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var escaped *ActivationTransaction
	if err := store.WithReservation(context.Background(), reservation, func(tx *ActivationTransaction) error {
		escaped = tx
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	journal := testJournal()
	journal.State = "prepared"
	if err := escaped.Prepare(journal); !errors.Is(err, ErrLifecycleConflict) {
		t.Fatalf("escaped transaction error = %v", err)
	}
	if _, err := os.Stat(store.paths.Journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escaped transaction mutated state: %v", err)
	}
}

func TestActivePreflightReservationBlocksRecoverAndReconcile(t *testing.T) {
	store := testStore(t)
	journal := testJournal()
	writeActiveRecords(t, store, journal)
	reservation, err := store.Reserve(context.Background(), nonceFor('b'), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	environment := &fakeEnvironment{}
	listener := newFakeListener(proofFromJournal(journal))
	if _, err := store.Recover(context.Background(), environment, listener); !errors.Is(err, ErrLifecycleConflict) {
		t.Fatalf("recover error = %v", err)
	}
	if _, err := store.Reconcile(context.Background()); !errors.Is(err, ErrLifecycleConflict) {
		t.Fatalf("reconcile error = %v", err)
	}
	if listener.stopCalls != 0 || len(environment.requests) != 0 {
		t.Fatalf("reserved preflight was mutated: stop=%d CAS=%d", listener.stopCalls, len(environment.requests))
	}
	if err := store.CancelReservation(context.Background(), reservation); err != nil {
		t.Fatal(err)
	}
}

func TestAtomicWriteFaultsNeverExposeCorruptRecord(t *testing.T) {
	type faultCase struct {
		point      string
		occurrence int
	}
	cases := []faultCase{{"state.removed", 1}, {"state.remove.parent.synced", 1}}
	for _, point := range []string{"state.temp.opened", "state.temp.written", "state.temp.synced", "state.temp.closed", "state.renamed", "state.parent.synced"} {
		cases = append(cases, faultCase{point, 1}, faultCase{point, 2})
	}
	for _, testCase := range cases {
		name := testCase.point + "-occurrence-" + strconv.Itoa(testCase.occurrence)
		t.Run(name, func(t *testing.T) {
			var fail atomic.Bool
			var seen atomic.Int32
			fail.Store(true)
			store := testStoreWithOptions(t, WithFaultInjector(FaultFunc(func(actual string) error {
				if fail.Load() && actual == testCase.point && int(seen.Add(1)) == testCase.occurrence {
					return errors.New("injected " + testCase.point)
				}
				return nil
			})))
			journal := testJournal()
			fail.Store(false)
			writeActiveRecords(t, store, journal)
			reservation, err := store.Reserve(context.Background(), nonceFor('f'), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			fail.Store(true)
			next := *journal
			next.State = "recovery_required"
			next.UpdatedAt = next.UpdatedAt.Add(time.Second)
			_ = store.WithReservation(context.Background(), reservation, func(tx *ActivationTransaction) error {
				return tx.Transition(ExpectedActivation{Generation: journal.Generation, State: "active"}, &next, "process_environment")
			})
			fail.Store(false)
			_ = store.CancelReservation(context.Background(), reservation)
			reconciliation, err := store.Reconcile(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if reconciliation.State != ReconciliationActive && reconciliation.State != ReconciliationRecoveryRequired {
				t.Fatalf("unexpected reconciliation: %+v", reconciliation)
			}
			got, err := readJournal(store.paths.Journal, store.uid)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != "active" && got.State != "recovery_required" {
				t.Fatalf("unexpected state %s", got.State)
			}
			gotReceipt, receiptErr := readReceipt(store.paths.Receipt, store.uid)
			if receiptErr == nil {
				if err := ValidateBinding(got, gotReceipt); err != nil {
					t.Fatalf("exposed unbound records: %v", err)
				}
			} else if !errors.Is(receiptErr, ErrNotFound) {
				t.Fatalf("receipt is neither valid nor missing: %v", receiptErr)
			}
		})
	}
}

func TestClearRemovalAndDirectorySyncFaultsRemainReconcileable(t *testing.T) {
	type faultCase struct {
		point      string
		occurrence int
	}
	for _, testCase := range []faultCase{{"state.removed", 1}, {"state.removed", 2}, {"state.remove.parent.synced", 1}, {"state.remove.parent.synced", 2}} {
		name := testCase.point + "-occurrence-" + strconv.Itoa(testCase.occurrence)
		t.Run(name, func(t *testing.T) {
			var enabled atomic.Bool
			var seen atomic.Int32
			store := testStoreWithOptions(t, WithFaultInjector(FaultFunc(func(point string) error {
				if enabled.Load() && point == testCase.point && int(seen.Add(1)) == testCase.occurrence {
					return errors.New("injected removal fault")
				}
				return nil
			})))
			journal := testJournal()
			writeActiveRecords(t, store, journal)
			reservation, err := store.Reserve(context.Background(), nonceFor('c'), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			enabled.Store(true)
			err = store.WithReservation(context.Background(), reservation, func(tx *ActivationTransaction) error {
				return tx.Clear(ExpectedActivation{Generation: journal.Generation, State: "active"})
			})
			if err == nil {
				t.Fatal("fault was not reached")
			}
			enabled.Store(false)
			_ = store.CancelReservation(context.Background(), reservation)
			result, reconcileErr := store.Reconcile(context.Background())
			if reconcileErr != nil {
				t.Fatal(reconcileErr)
			}
			if result.State != ReconciliationInactive && result.State != ReconciliationActive {
				t.Fatalf("unexpected state after interrupted clear: %+v", result)
			}
		})
	}
}

func TestRecoverValidRecordsRestoresAndVerifiesListenerStop(t *testing.T) {
	store := testStore(t)
	journal := testJournal()
	writeActiveRecords(t, store, journal)
	environment := &fakeEnvironment{}
	listener := newFakeListener(proofFromJournal(journal))

	result, err := store.Recover(context.Background(), environment, listener)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RecoveryComplete || result.ListenerEvidence != "listener_stop_verified" || len(result.RestoredEnvironment) != 5 {
		t.Fatalf("unexpected recovery result: %+v", result)
	}
	if listener.stopCalls != 1 || len(environment.requests) != 5 {
		t.Fatalf("stop=%d CAS=%d", listener.stopCalls, len(environment.requests))
	}
	for _, path := range []string{store.paths.Journal, store.paths.Receipt} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("record remains at %s: %v", path, err)
		}
	}
}

func TestRecoverPIDReuseNeverStopsUnrelatedProcess(t *testing.T) {
	store := testStore(t)
	journal := testJournal()
	writeActiveRecords(t, store, journal)
	listener := newFakeListener(proofFromJournal(journal))
	listener.live.ProcessStartIdentity = "reused-pid-start"

	result, err := store.Recover(context.Background(), &fakeEnvironment{}, listener)
	if err != nil {
		t.Fatal(err)
	}
	if listener.stopCalls != 0 || result.ListenerEvidence != "recorded_listener_identity_absent" {
		t.Fatalf("unrelated process was not protected: %+v stop=%d", result, listener.stopCalls)
	}
}

func TestValidJournalRepairsCorruptReceipt(t *testing.T) {
	store := testStore(t)
	journal := testJournal()
	writeActiveRecords(t, store, journal)
	if err := os.WriteFile(store.paths.Receipt, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := store.Recover(context.Background(), &fakeEnvironment{}, newFakeListener(proofFromJournal(journal)))
	if err != nil {
		t.Fatal(err)
	}
	if !result.ReceiptRepaired || result.Status != RecoveryComplete {
		t.Fatalf("unexpected recovery result: %+v", result)
	}
}

func TestReconcileRepairsReceiptWithoutTouchingRuntime(t *testing.T) {
	store := testStore(t)
	journal := testJournal()
	writeActiveRecords(t, store, journal)
	if err := os.WriteFile(store.paths.Receipt, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := store.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.State != ReconciliationActive || !result.ReceiptRepaired || result.ActivationID != journal.ActivationID {
		t.Fatalf("unexpected reconciliation: %+v", result)
	}
	repaired, err := readReceipt(store.paths.Receipt, store.uid)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateBinding(journal, repaired); err != nil {
		t.Fatal(err)
	}
}

func TestReceiptOnlyStopsListenerButNeverRestoresEnvironment(t *testing.T) {
	store := testStore(t)
	journal := testJournal()
	receipt := writeActiveRecords(t, store, journal)
	if err := os.WriteFile(store.paths.Journal, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := &fakeEnvironment{}
	listener := newFakeListener(proofFromReceipt(receipt))
	result, err := store.Recover(context.Background(), environment, listener)
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("recovery error = %v", err)
	}
	if result.Status != RecoveryRequired || listener.stopCalls != 1 || len(environment.requests) != 0 || len(result.ManualRemediation) != 5 {
		t.Fatalf("unexpected receipt-only result: %+v stop=%d CAS=%d", result, listener.stopCalls, len(environment.requests))
	}
}

func TestBothCorruptMutatesNothing(t *testing.T) {
	store := testStore(t)
	journal := testJournal()
	writeActiveRecords(t, store, journal)
	if err := os.WriteFile(store.paths.Journal, []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.paths.Receipt, []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := &fakeEnvironment{}
	listener := newFakeListener(proofFromJournal(journal))
	_, err := store.Recover(context.Background(), environment, listener)
	if !errors.Is(err, ErrOwnershipAmbiguous) || listener.stopCalls != 0 || len(environment.requests) != 0 {
		t.Fatalf("error=%v stop=%d CAS=%d", err, listener.stopCalls, len(environment.requests))
	}
}

func TestCASConflictPersistsBoundRecoveryRecords(t *testing.T) {
	store := testStore(t)
	journal := testJournal()
	writeActiveRecords(t, store, journal)
	environment := &fakeEnvironment{conflict: map[string]bool{"OPENAI_API_KEY": true}}
	result, err := store.Recover(context.Background(), environment, newFakeListener(proofFromJournal(journal)))
	if !errors.Is(err, ErrRecoveryRequired) || result.Status != RecoveryRequired {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	persistedJournal, err := readJournal(store.paths.Journal, store.uid)
	if err != nil {
		t.Fatal(err)
	}
	persistedReceipt, err := readReceipt(store.paths.Receipt, store.uid)
	if err != nil {
		t.Fatal(err)
	}
	if persistedJournal.State != "recovery_required" || persistedReceipt.State != "recovery_required" {
		t.Fatalf("journal=%s receipt=%s", persistedJournal.State, persistedReceipt.State)
	}
	if err := ValidateBinding(persistedJournal, persistedReceipt); err != nil {
		t.Fatal(err)
	}
}

func TestUnverifiedStopRequiresRecovery(t *testing.T) {
	store := testStore(t)
	journal := testJournal()
	writeActiveRecords(t, store, journal)
	listener := newFakeListener(proofFromJournal(journal))
	listener.keepLive = true
	result, err := store.Recover(context.Background(), &fakeEnvironment{}, listener)
	if !errors.Is(err, ErrRecoveryRequired) || result.ListenerEvidence != "listener_stop_unverified" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if len(result.ManualRemediation) == 0 || !strings.Contains(result.ManualRemediation[0], "do not signal a PID") {
		t.Fatalf("unsafe or missing listener remediation: %+v", result.ManualRemediation)
	}
}

func TestSecureFilesRejectSymlinkAndHardlink(t *testing.T) {
	store := testStore(t)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, store.paths.Journal); err != nil {
		t.Fatal(err)
	}
	if _, err := readJournal(store.paths.Journal, os.Getuid()); !errors.Is(err, ErrOwnershipAmbiguous) {
		t.Fatalf("symlink error = %v", err)
	}
	if err := os.Remove(store.paths.Journal); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, store.paths.Journal); err != nil {
		t.Fatal(err)
	}
	if _, err := readJournal(store.paths.Journal, os.Getuid()); !errors.Is(err, ErrOwnershipAmbiguous) {
		t.Fatalf("hardlink error = %v", err)
	}
}

func testStore(t *testing.T) *Store { return testStoreWithOptions(t) }

func testStoreWithOptions(t *testing.T, options ...Option) *Store {
	t.Helper()
	root := filepath.Join(t.TempDir(), "account", ".local", "share", "blazn", "proxy")
	store, err := newAt(root, os.Getuid(), options...)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func testJournal() *Journal {
	now := time.Date(2026, 8, 22, 6, 0, 0, 0, time.UTC)
	prior := "prior-value"
	environment := make([]JournalEnvironment, 0, len(EnvironmentNames))
	for index, name := range EnvironmentNames {
		item := JournalEnvironment{Name: name, DesiredValueDigest: testDigest, ActivationMarker: "marker-abcdefghijklmnop", RollbackAction: "remove_blazn_value"}
		if index == 0 {
			item.PriorPresent = true
			item.PriorValue = &prior
			item.RollbackAction = "restore_prior_value"
		}
		environment = append(environment, item)
	}
	return &Journal{
		SchemaVersion: SchemaVersion, ActivationID: "11111111-1111-4111-8111-111111111111", Nonce: nonceFor('n'), Generation: 1,
		State: "active", OwnerUID: os.Getuid(), Platform: "linux", Mode: "scoped_run", SessionIdentity: "session-1",
		Policy:      PolicyIdentity{ID: "22222222-2222-4222-8222-222222222222", Version: 1, Digest: testDigest},
		Binary:      BinaryIdentity{Path: "/usr/local/bin/blazn", Digest: testDigest},
		Listener:    ListenerIdentity{PID: 1234, ProcessStartIdentity: "start-1", ExecutableIdentity: "dev:inode", Address: "127.0.0.1:8123", ListenerKeyFingerprint: testDigest},
		Environment: environment, RollbackActions: []RollbackAction{{Ordinal: 1, Operation: "stop_listener", Target: "listener"}, {Ordinal: 2, Operation: "restore_environment", Target: "published variables"}},
		CreatedAt: now, UpdatedAt: now,
	}
}

func writeActiveRecords(t *testing.T, store *Store, journal *Journal) *Receipt {
	t.Helper()
	journal.State = "prepared"
	prepareReservation, err := store.Reserve(context.Background(), nonceFor('p'), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WithReservation(context.Background(), prepareReservation, func(tx *ActivationTransaction) error {
		return tx.Prepare(journal)
	}); err != nil {
		t.Fatal(err)
	}
	next := *journal
	next.State = "active"
	next.UpdatedAt = next.UpdatedAt.Add(time.Second)
	activeReservation, err := store.Reserve(context.Background(), nonceFor('a'), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WithReservation(context.Background(), activeReservation, func(tx *ActivationTransaction) error {
		return tx.Transition(ExpectedActivation{Generation: journal.Generation, State: "prepared"}, &next, "process_environment")
	}); err != nil {
		t.Fatal(err)
	}
	*journal = next
	receipt, err := readReceipt(store.paths.Receipt, store.uid)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func nonceFor(value byte) string { return string(makeBytes(value, 40)) }

func makeBytes(value byte, count int) []byte {
	result := make([]byte, count)
	for index := range result {
		result[index] = value
	}
	return result
}

type fakeEnvironment struct {
	requests []CompareAndSetRequest
	conflict map[string]bool
}

func (f *fakeEnvironment) CompareAndSet(_ context.Context, request CompareAndSetRequest) (CompareAndSetResult, error) {
	f.requests = append(f.requests, request)
	if f.conflict[request.Name] {
		return CASConflict, nil
	}
	return CASRestored, nil
}

type fakeListener struct {
	live      LiveListenerProof
	present   bool
	keepLive  bool
	stopCalls int
}

func newFakeListener(proof LiveListenerProof) *fakeListener {
	return &fakeListener{live: proof, present: true}
}

func (f *fakeListener) Inspect(context.Context, int) (LiveListenerProof, bool, error) {
	return f.live, f.present, nil
}

func (f *fakeListener) Stop(_ context.Context, proof LiveListenerProof) error {
	if proof != f.live {
		return errors.New("attempted to stop a mismatched listener")
	}
	f.stopCalls++
	if !f.keepLive {
		f.present = false
	}
	return nil
}

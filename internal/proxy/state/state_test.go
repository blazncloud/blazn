//go:build darwin || linux

package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

	err := store.WithLifecycleLock(context.Background(), func(locked *LockedStore) error {
		readJournal, err := locked.ReadJournal()
		if err != nil {
			return err
		}
		readReceipt, err := locked.ReadReceipt()
		if err != nil {
			return err
		}
		if err := ValidateBinding(readJournal, readReceipt); err != nil {
			return err
		}
		if readJournal.Checksum == "" || readReceipt.Checksum == "" || receipt.Checksum == "" {
			t.Fatal("records were not checksummed")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
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
	if err := store.WithReservation(context.Background(), changed, func(*LockedStore) error { return nil }); !errors.Is(err, ErrLifecycleConflict) {
		t.Fatalf("mismatched reservation error = %v", err)
	}
	callbackErr := errors.New("publication failed")
	if err := store.WithReservation(context.Background(), reservation, func(*LockedStore) error { return callbackErr }); !errors.Is(err, callbackErr) {
		t.Fatalf("callback error = %v", err)
	}
	if err := store.WithReservation(context.Background(), reservation, func(*LockedStore) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.paths.Reservation); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reservation was not consumed: %v", err)
	}
}

func TestAtomicWriteFaultsNeverExposeCorruptRecord(t *testing.T) {
	points := []string{"state.temp.opened", "state.temp.written", "state.temp.synced", "state.temp.closed", "state.renamed", "state.parent.synced"}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			var fail atomic.Bool
			fail.Store(true)
			store := testStoreWithOptions(t, WithFaultInjector(FaultFunc(func(actual string) error {
				if fail.Load() && actual == point {
					return errors.New("injected " + point)
				}
				return nil
			})))
			journal := testJournal()
			fail.Store(false)
			if err := store.WithLifecycleLock(context.Background(), func(locked *LockedStore) error { return locked.WriteJournal(journal) }); err != nil {
				t.Fatal(err)
			}
			fail.Store(true)
			journal.Generation = 2
			journal.UpdatedAt = journal.UpdatedAt.Add(time.Second)
			_ = store.WithLifecycleLock(context.Background(), func(locked *LockedStore) error { return locked.WriteJournal(journal) })
			fail.Store(false)
			if err := store.WithLifecycleLock(context.Background(), func(locked *LockedStore) error {
				got, err := locked.ReadJournal()
				if err != nil {
					return err
				}
				if got.Generation != 1 && got.Generation != 2 {
					t.Fatalf("unexpected generation %d", got.Generation)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
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
	if err := store.WithLifecycleLock(context.Background(), func(locked *LockedStore) error {
		journal, err := locked.ReadJournal()
		if err != nil {
			return err
		}
		receipt, err := locked.ReadReceipt()
		if err != nil {
			return err
		}
		if journal.State != "recovery_required" || receipt.State != "recovery_required" {
			t.Fatalf("journal=%s receipt=%s", journal.State, receipt.State)
		}
		return ValidateBinding(journal, receipt)
	}); err != nil {
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
	store, err := NewAt(root, os.Getuid(), options...)
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
	var receipt *Receipt
	if err := store.WithLifecycleLock(context.Background(), func(locked *LockedStore) error {
		if err := locked.WriteJournal(journal); err != nil {
			return err
		}
		var err error
		receipt, err = ReceiptFromJournal(journal, "process_environment")
		if err != nil {
			return err
		}
		return locked.WriteReceipt(receipt)
	}); err != nil {
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

//go:build darwin || linux

package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"sync"
	"time"
)

const maxRecordSize = 1 << 20

type Store struct {
	paths  Paths
	uid    int
	faults FaultInjector
	now    func() time.Time
}

type Option func(*Store)

func WithFaultInjector(faults FaultInjector) Option {
	return func(store *Store) {
		if faults != nil {
			store.faults = faults
		}
	}
}

func WithClock(now func() time.Time) Option {
	return func(store *Store) {
		if now != nil {
			store.now = now
		}
	}
}

func NewForCurrentAccount(options ...Option) (*Store, error) {
	paths, err := AccountPaths(runtime.GOOS, os.Getuid())
	if err != nil {
		return nil, err
	}
	return newStore(paths, os.Getuid(), options...)
}

// newAt is intentionally unexported. Production callers cannot redirect state
// away from the OS-account-derived root.
func newAt(root string, uid int, options ...Option) (*Store, error) {
	return newStore(pathsAt(root), uid, options...)
}

func newStore(paths Paths, uid int, options ...Option) (*Store, error) {
	store := &Store{paths: paths, uid: uid, faults: noFaults{}, now: time.Now}
	for _, option := range options {
		option(store)
	}
	if err := ensureSecureRoot(paths.Root, uid); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Paths() Paths { return s.paths }

type lockedStore struct{ store *Store }

func (s *Store) withLifecycleLock(ctx context.Context, operation func(*lockedStore) error) (err error) {
	lock, err := lockFile(ctx, s.paths.Lock, s.uid)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlockFile(lock)) }()
	return operation(&lockedStore{store: s})
}

type Reservation struct {
	Nonce     string    `json:"nonce"`
	OwnerUID  int       `json:"ownerUid"`
	ExpiresAt time.Time `json:"expiresAt"`
	Checksum  string    `json:"checksum"`
}

func (s *Store) Reserve(ctx context.Context, nonce string, ttl time.Duration) (Reservation, error) {
	if !validNonce(nonce) || ttl <= 0 || ttl > 5*time.Minute {
		return Reservation{}, fmt.Errorf("%w: invalid reservation", ErrInvalidState)
	}
	reservation := Reservation{Nonce: nonce, OwnerUID: s.uid, ExpiresAt: s.now().UTC().Add(ttl)}
	err := s.withLifecycleLock(ctx, func(locked *lockedStore) error {
		current, err := locked.readReservation()
		if err == nil && current.ExpiresAt.After(s.now()) {
			if current.Nonce != nonce {
				return ErrLifecycleConflict
			}
			reservation = *current
			return nil
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		return locked.writeReservation(&reservation)
	})
	return reservation, err
}

func (s *Store) withInternalReservation(ctx context.Context, operation func(*lockedStore) error) error {
	nonce, err := randomSuffix()
	if err != nil {
		return err
	}
	reservation, err := s.Reserve(ctx, nonce, 30*time.Second)
	if err != nil {
		return err
	}
	return s.withReservation(ctx, reservation, operation)
}

type ActivationTransaction struct {
	mu     sync.Mutex
	locked *lockedStore
	active bool
}

// WithReservation reacquires the lifecycle lock after network/provider checks,
// verifies the exact owner/nonce/expiry/checksum fence, and consumes the
// reservation only after the transition callback succeeds. A failed callback
// preserves it for explicit cancellation or expiry-based recovery.
func (s *Store) WithReservation(ctx context.Context, reservation Reservation, operation func(*ActivationTransaction) error) error {
	if operation == nil {
		return ErrInvalidState
	}
	return s.withReservation(ctx, reservation, func(locked *lockedStore) error {
		tx := &ActivationTransaction{locked: locked, active: true}
		defer func() {
			tx.mu.Lock()
			tx.active = false
			tx.mu.Unlock()
		}()
		return operation(tx)
	})
}

func (s *Store) withReservation(ctx context.Context, reservation Reservation, operation func(*lockedStore) error) error {
	return s.withLifecycleLock(ctx, func(locked *lockedStore) error {
		current, err := locked.readReservation()
		if err != nil {
			return fmt.Errorf("%w: reservation missing", ErrLifecycleConflict)
		}
		if current.Nonce != reservation.Nonce || current.OwnerUID != s.uid || reservation.OwnerUID != s.uid ||
			current.Checksum != reservation.Checksum || !current.ExpiresAt.Equal(reservation.ExpiresAt) || !current.ExpiresAt.After(s.now()) {
			return fmt.Errorf("%w: reservation changed or expired", ErrLifecycleConflict)
		}
		if err := operation(locked); err != nil {
			return err
		}
		return removeSecureFile(s.paths.Reservation, s.uid, s.faults)
	})
}

func (s *Store) CancelReservation(ctx context.Context, reservation Reservation) error {
	return s.withLifecycleLock(ctx, func(locked *lockedStore) error {
		current, err := locked.readReservation()
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.Nonce != reservation.Nonce || current.OwnerUID != s.uid || reservation.OwnerUID != s.uid || current.Checksum != reservation.Checksum {
			return ErrLifecycleConflict
		}
		return removeSecureFile(s.paths.Reservation, s.uid, s.faults)
	})
}

func (locked *lockedStore) writeJournal(journal *Journal) error {
	if journal.OwnerUID != locked.store.uid {
		return ErrOwnershipAmbiguous
	}
	if err := journal.Validate(); err != nil {
		return err
	}
	data, err := marshalChecksummed(journal, func(checksum string) { journal.Checksum = checksum })
	if err != nil {
		return err
	}
	return atomicWrite(locked.store.paths.Journal, locked.store.uid, data, locked.store.faults)
}

func (locked *lockedStore) writeReceipt(receipt *Receipt) error {
	if receipt.OwnerUID != locked.store.uid {
		return ErrOwnershipAmbiguous
	}
	if err := receipt.Validate(); err != nil {
		return err
	}
	data, err := marshalChecksummed(receipt, func(checksum string) { receipt.Checksum = checksum })
	if err != nil {
		return err
	}
	return atomicWrite(locked.store.paths.Receipt, locked.store.uid, data, locked.store.faults)
}

func (locked *lockedStore) readJournal() (*Journal, error) {
	return readJournal(locked.store.paths.Journal, locked.store.uid)
}

func (locked *lockedStore) readReceipt() (*Receipt, error) {
	return readReceipt(locked.store.paths.Receipt, locked.store.uid)
}

func (locked *lockedStore) removeRecords() error {
	if err := removeSecureFile(locked.store.paths.Receipt, locked.store.uid, locked.store.faults); err != nil {
		return err
	}
	return removeSecureFile(locked.store.paths.Journal, locked.store.uid, locked.store.faults)
}

type ExpectedActivation struct {
	Generation int64
	State      string
}

// Prepare creates the authoritative journal only when no prior activation
// evidence exists. It intentionally does not create a receipt until runtime
// publication becomes active.
func (tx *ActivationTransaction) Prepare(journal *Journal) error {
	if tx == nil {
		return ErrInvalidState
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if !tx.active || tx.locked == nil || journal == nil {
		return ErrLifecycleConflict
	}
	if journal.State != "prepared" {
		return fmt.Errorf("%w: initial journal state must be prepared", ErrInvalidState)
	}
	currentJournal, journalErr := tx.locked.readJournal()
	currentReceipt, receiptErr := tx.locked.readReceipt()
	if errors.Is(journalErr, ErrNotFound) && errors.Is(receiptErr, ErrNotFound) {
		return tx.locked.writeJournal(journal)
	}
	if ownershipError(journalErr) || ownershipError(receiptErr) || errors.Is(journalErr, ErrInvalidState) || errors.Is(receiptErr, ErrInvalidState) {
		return ErrOwnershipAmbiguous
	}
	if currentJournal != nil || currentReceipt != nil {
		return ErrLifecycleConflict
	}
	return errors.Join(journalErr, receiptErr)
}

// Transition advances a journal using compare-and-set generation/state. When
// the journal digest changes, any old receipt is removed and directory-synced
// first. Every crash point therefore leaves either a bound pair or an
// authoritative journal with a repairable missing receipt.
func (tx *ActivationTransaction) Transition(expected ExpectedActivation, next *Journal, mechanism string) error {
	if tx == nil {
		return ErrInvalidState
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if !tx.active || tx.locked == nil || next == nil {
		return ErrLifecycleConflict
	}
	current, err := tx.locked.readJournal()
	if err != nil {
		return err
	}
	if current.Generation != expected.Generation || current.State != expected.State {
		return ErrLifecycleConflict
	}
	if next.Generation != current.Generation || !sameActivationIdentity(current, next) || !validStateTransition(current.State, next.State) {
		return fmt.Errorf("%w: invalid activation transition", ErrInvalidState)
	}
	receipt, receiptErr := tx.locked.readReceipt()
	if ownershipError(receiptErr) {
		return ErrOwnershipAmbiguous
	}
	if receiptErr == nil {
		if err := ValidateBinding(current, receipt); err != nil {
			return err
		}
	} else if !errors.Is(receiptErr, ErrNotFound) && !errors.Is(receiptErr, ErrInvalidState) {
		return receiptErr
	}
	activatedAt := next.UpdatedAt
	if receiptErr == nil {
		activatedAt = receipt.ActivatedAt
		if mechanism == "" {
			mechanism = receipt.PublicationMechanism
		}
	}
	if !errors.Is(receiptErr, ErrNotFound) {
		if err := removeSecureFile(tx.locked.store.paths.Receipt, tx.locked.store.uid, tx.locked.store.faults); err != nil {
			return err
		}
	}
	if err := tx.locked.writeJournal(next); err != nil {
		return err
	}
	if next.State != "active" && next.State != "recovery_required" {
		return nil
	}
	newReceipt, err := receiptFromJournalAt(next, mechanism, activatedAt)
	if err != nil {
		return err
	}
	if next.State == "recovery_required" {
		newReceipt.State = "recovery_required"
	}
	return tx.locked.writeReceipt(newReceipt)
}

// Clear removes a bound activation using compare-and-set state. Receipt-first
// deletion makes every interruption recoverable from the journal.
func (tx *ActivationTransaction) Clear(expected ExpectedActivation) error {
	if tx == nil {
		return ErrInvalidState
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if !tx.active || tx.locked == nil {
		return ErrLifecycleConflict
	}
	journal, err := tx.locked.readJournal()
	if err != nil {
		return err
	}
	if journal.Generation != expected.Generation || journal.State != expected.State {
		return ErrLifecycleConflict
	}
	receipt, receiptErr := tx.locked.readReceipt()
	if ownershipError(receiptErr) {
		return ErrOwnershipAmbiguous
	}
	if receiptErr == nil {
		if err := ValidateBinding(journal, receipt); err != nil {
			return err
		}
	} else if !errors.Is(receiptErr, ErrNotFound) && !errors.Is(receiptErr, ErrInvalidState) {
		return receiptErr
	}
	return tx.locked.removeRecords()
}

func sameActivationIdentity(current, next *Journal) bool {
	return current.SchemaVersion == next.SchemaVersion && current.ActivationID == next.ActivationID && current.Nonce == next.Nonce &&
		current.OwnerUID == next.OwnerUID && current.Platform == next.Platform && current.Mode == next.Mode &&
		current.SessionIdentity == next.SessionIdentity && current.Policy == next.Policy && current.Binary == next.Binary &&
		current.Listener == next.Listener && current.CreatedAt.Equal(next.CreatedAt) &&
		reflect.DeepEqual(current.Environment, next.Environment) && reflect.DeepEqual(current.RollbackActions, next.RollbackActions)
}

func validStateTransition(current, next string) bool {
	allowed := map[string]map[string]bool{
		"prepared":          {"publishing": true, "active": true, "recovery_required": true},
		"publishing":        {"active": true, "recovery_required": true},
		"active":            {"deactivating": true, "recovery_required": true},
		"deactivating":      {"recovery_required": true},
		"recovery_required": {"deactivating": true},
	}
	return allowed[current][next]
}

type ReconciliationState string

const (
	ReconciliationInactive         ReconciliationState = "inactive"
	ReconciliationActive           ReconciliationState = "active"
	ReconciliationRecoveryRequired ReconciliationState = "recovery_required"
)

type Reconciliation struct {
	State           ReconciliationState
	ActivationID    string
	Generation      int64
	LifecycleState  string
	ReceiptRepaired bool
}

// Reconcile validates the redundant records without touching the listener or
// environment. A valid protected journal is authoritative and can repair a
// missing/corrupt receipt; a receipt can never repair a journal because it
// intentionally contains no prior values.
func (s *Store) Reconcile(ctx context.Context) (result Reconciliation, err error) {
	var semanticErr error
	err = s.withInternalReservation(ctx, func(locked *lockedStore) error {
		journal, journalErr := locked.readJournal()
		receipt, receiptErr := locked.readReceipt()
		if ownershipError(journalErr) || ownershipError(receiptErr) {
			return ErrOwnershipAmbiguous
		}
		if journalErr == nil && receiptErr == nil {
			if err := ValidateBinding(journal, receipt); err != nil {
				return err
			}
			result = reconciliationFromJournal(journal, false)
			return nil
		}
		if journalErr == nil {
			repaired, err := receiptFromJournal(journal, publicationMechanism(journal))
			if err != nil {
				return err
			}
			if journal.State != "active" {
				repaired.State = "recovery_required"
			}
			if err := locked.writeReceipt(repaired); err != nil {
				return err
			}
			result = reconciliationFromJournal(journal, true)
			return nil
		}
		if receiptErr == nil {
			result = Reconciliation{State: ReconciliationRecoveryRequired, ActivationID: receipt.ActivationID, Generation: receipt.Generation, LifecycleState: "recovery_required"}
			semanticErr = ErrRecoveryRequired
			return nil
		}
		if errors.Is(journalErr, ErrNotFound) && errors.Is(receiptErr, ErrNotFound) {
			result.State = ReconciliationInactive
			return nil
		}
		return ErrOwnershipAmbiguous
	})
	return result, errors.Join(err, semanticErr)
}

func reconciliationFromJournal(journal *Journal, repaired bool) Reconciliation {
	state := ReconciliationActive
	if journal.State != "active" {
		state = ReconciliationRecoveryRequired
	}
	return Reconciliation{
		State: state, ActivationID: journal.ActivationID, Generation: journal.Generation,
		LifecycleState: journal.State, ReceiptRepaired: repaired,
	}
}

func readJournal(path string, uid int) (*Journal, error) {
	var journal Journal
	if err := readRecord(path, uid, &journal); err != nil {
		return nil, err
	}
	if journal.OwnerUID != uid {
		return nil, ErrOwnershipAmbiguous
	}
	if err := journal.Validate(); err != nil {
		return nil, err
	}
	if err := verifyChecksum(&journal, journal.Checksum); err != nil {
		return nil, err
	}
	return &journal, nil
}

func readReceipt(path string, uid int) (*Receipt, error) {
	var receipt Receipt
	if err := readRecord(path, uid, &receipt); err != nil {
		return nil, err
	}
	if receipt.OwnerUID != uid {
		return nil, ErrOwnershipAmbiguous
	}
	if err := receipt.Validate(); err != nil {
		return nil, err
	}
	if err := verifyChecksum(&receipt, receipt.Checksum); err != nil {
		return nil, err
	}
	return &receipt, nil
}

func readRecord(path string, uid int, destination any) error {
	data, err := readSecureFile(path, uid, maxRecordSize)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidState, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing record content", ErrInvalidState)
	}
	return nil
}

func (locked *lockedStore) readReservation() (*Reservation, error) {
	var reservation Reservation
	if err := readRecord(locked.store.paths.Reservation, locked.store.uid, &reservation); err != nil {
		return nil, err
	}
	if reservation.OwnerUID != locked.store.uid || len(reservation.Nonce) < 32 || reservation.ExpiresAt.IsZero() {
		return nil, ErrInvalidState
	}
	if err := verifyChecksum(&reservation, reservation.Checksum); err != nil {
		return nil, err
	}
	return &reservation, nil
}

func (locked *lockedStore) writeReservation(reservation *Reservation) error {
	data, err := marshalChecksummed(reservation, func(checksum string) { reservation.Checksum = checksum })
	if err != nil {
		return err
	}
	return atomicWrite(locked.store.paths.Reservation, locked.store.uid, data, locked.store.faults)
}

func ValidateBinding(journal *Journal, receipt *Receipt) error {
	if receipt.ActivationID != journal.ActivationID || receipt.Nonce != journal.Nonce || receipt.Generation != journal.Generation ||
		receipt.OwnerUID != journal.OwnerUID || receipt.JournalDigest != journal.Checksum || receipt.PolicyDigest != journal.Policy.Digest ||
		receipt.Platform != journal.Platform || receipt.Mode != journal.Mode || receipt.SessionIdentity != journal.SessionIdentity ||
		receipt.Binary != journal.Binary || receipt.Listener != journal.Listener {
		return ErrOwnershipAmbiguous
	}
	if len(receipt.Environment) != len(journal.Environment) {
		return ErrOwnershipAmbiguous
	}
	wanted := make(map[string]ReceiptEnvironment, len(journal.Environment))
	for _, item := range journal.Environment {
		wanted[item.Name] = ReceiptEnvironment{Name: item.Name, DesiredValueDigest: item.DesiredValueDigest, ActivationMarker: item.ActivationMarker}
	}
	for _, item := range receipt.Environment {
		if wanted[item.Name] != item {
			return ErrOwnershipAmbiguous
		}
	}
	return nil
}

func receiptFromJournal(journal *Journal, mechanism string) (*Receipt, error) {
	return receiptFromJournalAt(journal, mechanism, journal.UpdatedAt)
}

func receiptFromJournalAt(journal *Journal, mechanism string, activatedAt time.Time) (*Receipt, error) {
	if journal.Checksum == "" || validateDigest(journal.Checksum) != nil {
		return nil, fmt.Errorf("%w: journal must be checksummed first", ErrInvalidState)
	}
	environment := make([]ReceiptEnvironment, 0, len(journal.Environment))
	for _, item := range journal.Environment {
		environment = append(environment, ReceiptEnvironment{Name: item.Name, DesiredValueDigest: item.DesiredValueDigest, ActivationMarker: item.ActivationMarker})
	}
	receipt := &Receipt{
		SchemaVersion: SchemaVersion, ActivationID: journal.ActivationID, Nonce: journal.Nonce, Generation: journal.Generation,
		OwnerUID: journal.OwnerUID, JournalDigest: journal.Checksum, PolicyDigest: journal.Policy.Digest,
		Platform: journal.Platform, Mode: journal.Mode, SessionIdentity: journal.SessionIdentity, Binary: journal.Binary,
		Listener: journal.Listener, PublicationMechanism: mechanism, Environment: environment,
		RollbackSummary: append([]RollbackAction(nil), journal.RollbackActions...), ActivatedAt: activatedAt, State: "active",
	}
	return receipt, receipt.Validate()
}

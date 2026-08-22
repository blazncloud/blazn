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
	"runtime"
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

type LockedStore struct{ store *Store }

func (s *Store) WithLifecycleLock(ctx context.Context, operation func(*LockedStore) error) (err error) {
	lock, err := lockFile(ctx, s.paths.Lock, s.uid)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlockFile(lock)) }()
	return operation(&LockedStore{store: s})
}

type Reservation struct {
	Nonce     string    `json:"nonce"`
	OwnerUID  int       `json:"ownerUid"`
	ExpiresAt time.Time `json:"expiresAt"`
	Checksum  string    `json:"checksum"`
}

func (s *Store) Reserve(ctx context.Context, nonce string, ttl time.Duration) (Reservation, error) {
	if len(nonce) < 32 || ttl <= 0 || ttl > 5*time.Minute {
		return Reservation{}, fmt.Errorf("%w: invalid reservation", ErrInvalidState)
	}
	reservation := Reservation{Nonce: nonce, OwnerUID: s.uid, ExpiresAt: s.now().UTC().Add(ttl)}
	err := s.WithLifecycleLock(ctx, func(locked *LockedStore) error {
		current, err := locked.readReservation()
		if err == nil && current.ExpiresAt.After(s.now()) && current.Nonce != nonce {
			return ErrLifecycleConflict
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		return locked.writeReservation(&reservation)
	})
	return reservation, err
}

// WithReservation reacquires the lifecycle lock after network/provider checks,
// verifies the exact nonce and expiry, and consumes the reservation only after
// the mutation callback succeeds. A failed callback preserves it for recovery.
func (s *Store) WithReservation(ctx context.Context, reservation Reservation, operation func(*LockedStore) error) error {
	return s.WithLifecycleLock(ctx, func(locked *LockedStore) error {
		current, err := locked.readReservation()
		if err != nil {
			return fmt.Errorf("%w: reservation missing", ErrLifecycleConflict)
		}
		if current.Nonce != reservation.Nonce || current.OwnerUID != s.uid || !current.ExpiresAt.Equal(reservation.ExpiresAt) || !current.ExpiresAt.After(s.now()) {
			return fmt.Errorf("%w: reservation changed or expired", ErrLifecycleConflict)
		}
		if err := operation(locked); err != nil {
			return err
		}
		return removeSecureFile(s.paths.Reservation, s.uid, s.faults)
	})
}

func (s *Store) CancelReservation(ctx context.Context, reservation Reservation) error {
	return s.WithLifecycleLock(ctx, func(locked *LockedStore) error {
		current, err := locked.readReservation()
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.Nonce != reservation.Nonce || current.OwnerUID != s.uid {
			return ErrLifecycleConflict
		}
		return removeSecureFile(s.paths.Reservation, s.uid, s.faults)
	})
}

func (locked *LockedStore) WriteJournal(journal *Journal) error {
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

func (locked *LockedStore) WriteReceipt(receipt *Receipt) error {
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

func (locked *LockedStore) ReadJournal() (*Journal, error) {
	return readJournal(locked.store.paths.Journal, locked.store.uid)
}

func (locked *LockedStore) ReadReceipt() (*Receipt, error) {
	return readReceipt(locked.store.paths.Receipt, locked.store.uid)
}

func (locked *LockedStore) RemoveRecords() error {
	if err := removeSecureFile(locked.store.paths.Receipt, locked.store.uid, locked.store.faults); err != nil {
		return err
	}
	return removeSecureFile(locked.store.paths.Journal, locked.store.uid, locked.store.faults)
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
	err = s.WithLifecycleLock(ctx, func(locked *LockedStore) error {
		journal, journalErr := locked.ReadJournal()
		receipt, receiptErr := locked.ReadReceipt()
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
			repaired, err := ReceiptFromJournal(journal, publicationMechanism(journal))
			if err != nil {
				return err
			}
			if journal.State != "active" {
				repaired.State = "recovery_required"
			}
			if err := locked.WriteReceipt(repaired); err != nil {
				return err
			}
			result = reconciliationFromJournal(journal, true)
			return nil
		}
		if receiptErr == nil {
			result = Reconciliation{State: ReconciliationRecoveryRequired, ActivationID: receipt.ActivationID, Generation: receipt.Generation, LifecycleState: "recovery_required"}
			return ErrRecoveryRequired
		}
		if errors.Is(journalErr, ErrNotFound) && errors.Is(receiptErr, ErrNotFound) {
			result.State = ReconciliationInactive
			return nil
		}
		return ErrOwnershipAmbiguous
	})
	return result, err
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

func (locked *LockedStore) readReservation() (*Reservation, error) {
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

func (locked *LockedStore) writeReservation(reservation *Reservation) error {
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

func ReceiptFromJournal(journal *Journal, mechanism string) (*Receipt, error) {
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
		RollbackSummary: append([]RollbackAction(nil), journal.RollbackActions...), ActivatedAt: journal.UpdatedAt, State: "active",
	}
	return receipt, receipt.Validate()
}

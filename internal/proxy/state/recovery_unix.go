//go:build darwin || linux

package state

import (
	"context"
	"errors"
)

type CompareAndSetRequest struct {
	Name                string
	ExpectedValueDigest string
	ActivationMarker    string
	PriorPresent        bool
	PriorValue          *string
}

type CompareAndSetResult string

const (
	CASRestored        CompareAndSetResult = "restored"
	CASAlreadyRestored CompareAndSetResult = "already_restored"
	CASConflict        CompareAndSetResult = "conflict"
)

type EnvironmentRestorer interface {
	CompareAndSet(context.Context, CompareAndSetRequest) (CompareAndSetResult, error)
}

// LiveListenerProof is returned by a platform-specific, authenticated live
// identity query. Generation/mode/session/nonce are listener-owned metadata,
// not caller claims or PID-only evidence.
type LiveListenerProof struct {
	PID                    int
	ProcessStartIdentity   string
	ExecutableIdentity     string
	BinaryDigest           string
	ListenerKeyFingerprint string
	ActivationNonce        string
	OwnerUID               int
	Generation             int64
	Mode                   string
	SessionIdentity        string
}

type ListenerController interface {
	Inspect(context.Context, int) (LiveListenerProof, bool, error)
	Stop(context.Context, LiveListenerProof) error
}

type RecoveryStatus string

const (
	RecoveryNotActive RecoveryStatus = "not_active"
	RecoveryComplete  RecoveryStatus = "recovered"
	RecoveryRequired  RecoveryStatus = "recovery_required"
)

type RecoveryResult struct {
	Status                RecoveryStatus
	ListenerEvidence      string
	RestoredEnvironment   []string
	ConflictedEnvironment []string
	ManualRemediation     []string
	ReceiptRepaired       bool
}

func (s *Store) Recover(ctx context.Context, environment EnvironmentRestorer, listener ListenerController) (result RecoveryResult, err error) {
	err = s.WithLifecycleLock(ctx, func(locked *LockedStore) error {
		journal, journalErr := locked.ReadJournal()
		receipt, receiptErr := locked.ReadReceipt()

		if ownershipError(journalErr) || ownershipError(receiptErr) {
			return ErrOwnershipAmbiguous
		}
		journalValid := journalErr == nil
		receiptValid := receiptErr == nil

		if !journalValid && !receiptValid {
			if errors.Is(journalErr, ErrNotFound) && errors.Is(receiptErr, ErrNotFound) {
				result.Status = RecoveryNotActive
				return nil
			}
			return ErrOwnershipAmbiguous
		}
		if journalValid && receiptValid {
			if err := ValidateBinding(journal, receipt); err != nil {
				return err
			}
		}

		if !journalValid {
			return s.recoverFromReceiptOnly(ctx, locked, receipt, listener, &result)
		}

		if !receiptValid {
			mechanism := publicationMechanism(journal)
			repaired, err := ReceiptFromJournal(journal, mechanism)
			if err != nil {
				return err
			}
			if err := locked.WriteReceipt(repaired); err != nil {
				return err
			}
			receipt = repaired
			result.ReceiptRepaired = true
		}

		listenerOK := true
		evidence, stopErr := stopVerified(ctx, listener, proofFromJournal(journal))
		result.ListenerEvidence = evidence
		if stopErr != nil {
			listenerOK = false
		}

		environmentOK := true
		for _, item := range journal.Environment {
			cas, casErr := environment.CompareAndSet(ctx, CompareAndSetRequest{
				Name: item.Name, ExpectedValueDigest: item.DesiredValueDigest, ActivationMarker: item.ActivationMarker,
				PriorPresent: item.PriorPresent, PriorValue: item.PriorValue,
			})
			if casErr != nil || cas == CASConflict {
				environmentOK = false
				result.ConflictedEnvironment = append(result.ConflictedEnvironment, item.Name)
				continue
			}
			if cas != CASRestored && cas != CASAlreadyRestored {
				environmentOK = false
				result.ConflictedEnvironment = append(result.ConflictedEnvironment, item.Name)
				continue
			}
			result.RestoredEnvironment = append(result.RestoredEnvironment, item.Name)
		}

		if listenerOK && environmentOK {
			if err := locked.RemoveRecords(); err != nil {
				return err
			}
			result.Status = RecoveryComplete
			return nil
		}

		journal.State = "recovery_required"
		journal.UpdatedAt = s.now().UTC()
		if err := locked.WriteJournal(journal); err != nil {
			return err
		}
		recoveryReceipt, err := ReceiptFromJournal(journal, receipt.PublicationMechanism)
		if err != nil {
			return err
		}
		recoveryReceipt.State = "recovery_required"
		if err := locked.WriteReceipt(recoveryReceipt); err != nil {
			return err
		}
		result.Status = RecoveryRequired
		result.ManualRemediation = manualEnvironment(result.ConflictedEnvironment)
		return ErrRecoveryRequired
	})
	return result, err
}

func (s *Store) recoverFromReceiptOnly(ctx context.Context, locked *LockedStore, receipt *Receipt, listener ListenerController, result *RecoveryResult) error {
	evidence, stopErr := stopVerified(ctx, listener, proofFromReceipt(receipt))
	result.ListenerEvidence = evidence
	receipt.State = "recovery_required"
	if err := locked.WriteReceipt(receipt); err != nil {
		return err
	}
	result.Status = RecoveryRequired
	result.ManualRemediation = manualEnvironment(EnvironmentNames[:])
	if stopErr != nil {
		return errors.Join(ErrRecoveryRequired, stopErr)
	}
	return ErrRecoveryRequired
}

func ownershipError(err error) bool { return errors.Is(err, ErrOwnershipAmbiguous) }

func publicationMechanism(journal *Journal) string {
	if journal.Mode == "scoped_run" {
		return "process_environment"
	}
	if journal.Platform == "darwin" {
		return "launchctl_user_environment"
	}
	return "systemd_user_environment"
}

func proofFromJournal(journal *Journal) LiveListenerProof {
	return LiveListenerProof{
		PID: journal.Listener.PID, ProcessStartIdentity: journal.Listener.ProcessStartIdentity,
		ExecutableIdentity: journal.Listener.ExecutableIdentity, BinaryDigest: journal.Binary.Digest,
		ListenerKeyFingerprint: journal.Listener.ListenerKeyFingerprint, ActivationNonce: journal.Nonce,
		OwnerUID: journal.OwnerUID, Generation: journal.Generation, Mode: journal.Mode, SessionIdentity: journal.SessionIdentity,
	}
}

func proofFromReceipt(receipt *Receipt) LiveListenerProof {
	return LiveListenerProof{
		PID: receipt.Listener.PID, ProcessStartIdentity: receipt.Listener.ProcessStartIdentity,
		ExecutableIdentity: receipt.Listener.ExecutableIdentity, BinaryDigest: receipt.Binary.Digest,
		ListenerKeyFingerprint: receipt.Listener.ListenerKeyFingerprint, ActivationNonce: receipt.Nonce,
		OwnerUID: receipt.OwnerUID, Generation: receipt.Generation, Mode: receipt.Mode, SessionIdentity: receipt.SessionIdentity,
	}
}

func stopVerified(ctx context.Context, controller ListenerController, expected LiveListenerProof) (string, error) {
	current, live, err := controller.Inspect(ctx, expected.PID)
	if err != nil {
		return "listener_inspection_failed", err
	}
	if !live {
		return "listener_absent", nil
	}
	if current != expected {
		// PID reuse or any other identity change proves the recorded listener is
		// absent. Never signal the unrelated live process.
		return "recorded_listener_identity_absent", nil
	}
	if err := controller.Stop(ctx, expected); err != nil {
		return "listener_stop_failed", err
	}
	after, live, err := controller.Inspect(ctx, expected.PID)
	if err != nil {
		return "listener_stop_unverified", err
	}
	if live && after == expected {
		return "listener_stop_unverified", errors.New("listener identity remains live after stop")
	}
	return "listener_stop_verified", nil
}

func manualEnvironment(names []string) []string {
	result := make([]string, 0, len(names))
	for _, name := range names {
		if validEnvironmentName(name) {
			result = append(result, "inspect and manually restore or remove "+name+" for the recorded OS session")
		}
	}
	return result
}

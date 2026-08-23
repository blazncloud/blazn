package activation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/blazncloud/blazn/internal/proxy/state"
	"github.com/blazncloud/blazn/internal/proxycontract"
)

const ContractVersion = "proxy/v1alpha1"
const cleanupTimeout = 30 * time.Second

var (
	ErrUnavailable          = errors.New("proxy platform adapter is unavailable")
	ErrSessionUnsupported   = errors.New("proxy session activation is unsupported")
	ErrDifferentPolicy      = errors.New("proxy already active with a different policy")
	ErrDifferentScope       = errors.New("proxy already active in a different mode or OS session")
	ErrCARemovalUnsupported = errors.New("proxy CA removal is unsupported")
	ErrRecovery             = state.ErrRecoveryRequired
)

type PriorValue struct {
	Present bool
	Value   string
}
type PublishedValue struct{ Name, Value, Marker string }

type Environment interface {
	state.EnvironmentRestorer
	Platform() string
	SessionIdentity(context.Context) (string, error)
	ResolveMode(string) (string, string, error)
	Snapshot(context.Context, []string) (map[string]PriorValue, error)
	Publish(context.Context, string, []PublishedValue) error
	BaseEnvironment() []string
}

type ManagedListener interface {
	Identity() state.ListenerIdentity
	Inspect(context.Context) (state.LiveListenerProof, bool, error)
	ChildEnvironment([]string) ([]string, error)
	Shutdown(context.Context) error
}

type ListenerFactory interface {
	Start(context.Context, proxycontract.Policy, string, ListenerMetadata) (ManagedListener, error)
}

type ListenerMetadata struct {
	ActivationID, Nonce, Mode, SessionIdentity, BinaryDigest string
	Generation, OwnerUID                       int64
}

type PolicyLoader interface {
	Load(string) (proxycontract.Policy, string, error)
}
type PolicyLoaderFunc func(string) (proxycontract.Policy, string, error)

func (f PolicyLoaderFunc) Load(path string) (proxycontract.Policy, string, error) { return f(path) }

type Store interface {
	Activate(context.Context, *state.Journal, string, func() error) error
	Reconcile(context.Context) (state.Reconciliation, error)
	Recover(context.Context, state.EnvironmentRestorer, state.ListenerController) (state.RecoveryResult, error)
	RecoverExact(context.Context, state.EnvironmentRestorer, state.ListenerController, state.ExpectedRecovery) (state.RecoveryResult, error)
	AcquireScope(context.Context, string, int64) (*ScopeLease, error)
	RenewScope(context.Context, *ScopeLease) error
	ReleaseScope(context.Context, *ScopeLease) error
}
type ScopeLease struct {
	mu           sync.Mutex
	reservation  state.Reservation
	ActivationID string
	Generation   int64
}

type PersistentStore struct{ Value *state.Store }

func (p PersistentStore) Reconcile(ctx context.Context) (state.Reconciliation, error) {
	return p.Value.Reconcile(ctx)
}
func (p PersistentStore) Recover(ctx context.Context, environment state.EnvironmentRestorer, listener state.ListenerController) (state.RecoveryResult, error) {
	return p.Value.Recover(ctx, environment, listener)
}
func (p PersistentStore) RecoverExact(ctx context.Context, environment state.EnvironmentRestorer, listener state.ListenerController, expected state.ExpectedRecovery) (state.RecoveryResult, error) {
	return p.Value.RecoverExpected(ctx, environment, listener, expected)
}
func (p PersistentStore) AcquireScope(ctx context.Context, id string, generation int64) (*ScopeLease, error) {
	_, nonce, err := randomIdentity()
	if err != nil {
		return nil, err
	}
	reservation, err := p.Value.Reserve(ctx, nonce, 2*time.Minute)
	if err != nil {
		return nil, err
	}
	lease := &ScopeLease{reservation: reservation, ActivationID: id, Generation: generation}
	current, reconcileErr := p.Value.Reconcile(ctx)
	if reconcileErr != nil || current.State != state.ReconciliationActive || current.ActivationID != id || current.Generation != generation {
		_ = p.Value.CancelReservation(context.Background(), reservation)
		return nil, errors.Join(state.ErrLifecycleConflict, reconcileErr)
	}
	return lease, nil
}
func (p PersistentStore) RenewScope(ctx context.Context, lease *ScopeLease) error {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	next, err := p.Value.RenewReservation(ctx, lease.reservation, 2*time.Minute)
	if err == nil {
		lease.reservation = next
	}
	return err
}
func (p PersistentStore) ReleaseScope(ctx context.Context, lease *ScopeLease) error {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return p.Value.CancelReservation(ctx, lease.reservation)
}
func (p PersistentStore) Activate(ctx context.Context, journal *state.Journal, mechanism string, publish func() error) (err error) {
	reservation, err := p.Value.Reserve(ctx, journal.Nonce, 30*time.Second)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, p.Value.CancelReservation(context.Background(), reservation)) }()
	return p.Value.WithReservation(ctx, reservation, func(tx *state.ActivationTransaction) (callbackErr error) {
		if err := tx.Prepare(journal); err != nil {
			return err
		}
		publishing := *journal
		publishing.State = "publishing"
		publishing.UpdatedAt = journal.UpdatedAt.Add(time.Nanosecond)
		if err := tx.Transition(state.ExpectedActivation{Generation: journal.Generation, State: "prepared"}, &publishing, ""); err != nil {
			return err
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				callbackErr = fmt.Errorf("proxy publication panicked")
			}
			if callbackErr != nil {
				recovery := publishing
				recovery.State = "recovery_required"
				recovery.UpdatedAt = publishing.UpdatedAt.Add(time.Nanosecond)
				callbackErr = errors.Join(callbackErr, tx.Transition(state.ExpectedActivation{Generation: journal.Generation, State: "publishing"}, &recovery, mechanism))
			}
		}()
		if err := publish(); err != nil {
			return err
		}
		active := publishing
		active.State = "active"
		active.UpdatedAt = publishing.UpdatedAt.Add(time.Nanosecond)
		return tx.Transition(state.ExpectedActivation{Generation: journal.Generation, State: "publishing"}, &active, mechanism)
	})
}

type Runner interface {
	Run(context.Context, []string, []string) (int, error)
}
type Event = proxycontract.Event
type EventReader interface {
	Read(context.Context, string, bool) ([]Event, error)
}

type Dependencies struct {
	Store       Store
	Environment Environment
	Listeners   ListenerFactory
	Controller  state.ListenerController
	Policies    PolicyLoader
	Runner      Runner
	Events      EventReader
	Binary      state.BinaryIdentity
	OwnerUID    int
	Now         func() time.Time
}

type Service struct {
	deps         Dependencies
	mu           sync.Mutex
	lifecycleMu  sync.Mutex
	listeners    map[int]managedProof
	stopped      map[int]stoppedProof
	environments map[string]activationEnvironment
}
type managedProof struct {
	listener     ManagedListener
	proof        state.LiveListenerProof
	activationID string
}
type activationEnvironment struct {
	generation int64
	values     []string
}
type stoppedProof struct {
	activationID string
	proof        state.LiveListenerProof
}

type Result struct {
	Command            string          `json:"command"`
	ContractVersion    string          `json:"contractVersion"`
	Status             string          `json:"status"`
	State              string          `json:"state"`
	Timestamp          string          `json:"timestamp"`
	ActivationID       string          `json:"activationId,omitempty"`
	Generation         int64           `json:"generation,omitempty"`
	PolicyDigest       string          `json:"policyDigest,omitempty"`
	Mode               string          `json:"mode,omitempty"`
	ListenerAddress    string          `json:"listenerAddress,omitempty"`
	PublishedVariables []string        `json:"publishedVariables,omitempty"`
	ManualRemediation  []string        `json:"manualRemediation,omitempty"`
	Cleanup            *CleanupReceipt `json:"cleanup,omitempty"`
	ChildExitCode      *int            `json:"childExitCode,omitempty"`
	ExitCode           int             `json:"exitCode"`
}
type CleanupReceipt struct {
	Status              string   `json:"status"`
	ListenerEvidence    string   `json:"listenerEvidence"`
	RestoredVariables   []string `json:"restoredVariables"`
	ConflictedVariables []string `json:"conflictedVariables"`
}
type Route struct {
	ID                  string `json:"id"`
	DestinationClass    string `json:"destinationClass"`
	DestinationProtocol string `json:"destinationProtocol"`
	Endpoint            string `json:"endpoint"`
}

func New(deps Dependencies) (*Service, error) {
	if deps.Store == nil || deps.Environment == nil || deps.Listeners == nil || deps.Controller == nil || deps.Policies == nil || deps.Runner == nil || deps.Binary.Path == "" || deps.Binary.Digest == "" || deps.OwnerUID < 0 {
		return nil, ErrUnavailable
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Service{deps: deps, listeners: map[int]managedProof{}, stopped: map[int]stoppedProof{}, environments: map[string]activationEnvironment{}}, nil
}

func (s *Service) On(ctx context.Context, policyPath, requestedMode string) (Result, error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	policy, digest, err := s.deps.Policies.Load(policyPath)
	if err != nil {
		return s.result("proxy on", "failed", "inactive", 2), err
	}
	current, reconcileErr := s.deps.Store.Reconcile(ctx)
	if result, resultErr, handled := s.reconcileOn(ctx, current, reconcileErr, digest, requestedMode); handled {
		return result, resultErr
	}
	mode, mechanism, err := s.deps.Environment.ResolveMode(requestedMode)
	if err != nil {
		return s.result("proxy on", "unsupported", "inactive", 7), err
	}
	result, activateErr := s.activate(ctx, "proxy on", policy, digest, mode, mechanism)
	if !errors.Is(activateErr, state.ErrLifecycleConflict) {
		return result, activateErr
	}
	current, reconcileErr = s.deps.Store.Reconcile(ctx)
	if reconciled, resultErr, handled := s.reconcileOn(ctx, current, reconcileErr, digest, requestedMode); handled {
		return reconciled, resultErr
	}
	return s.result("proxy on", "recovery_required", "recovery_required", 9), errors.Join(ErrRecovery, activateErr)
}

func (s *Service) reconcileOn(ctx context.Context, current state.Reconciliation, reconcileErr error, digest, requestedMode string) (Result, error, bool) {
	if current.State == state.ReconciliationRecoveryRequired || errors.Is(reconcileErr, state.ErrRecoveryRequired) {
		return s.result("proxy on", "recovery_required", "recovery_required", 9), ErrRecovery, true
	}
	if reconcileErr != nil {
		return s.result("proxy on", "recovery_required", "recovery_required", 9), errors.Join(ErrRecovery, reconcileErr), true
	}
	if current.State == state.ReconciliationActive {
		if current.ListenerProof == nil {
			return s.result("proxy on", "recovery_required", "recovery_required", 9), ErrRecovery, true
		}
		observed, live, inspectErr := s.Inspect(ctx, current.ListenerProof.PID)
		if inspectErr != nil || !live || observed != *current.ListenerProof {
			return s.result("proxy on", "recovery_required", "recovery_required", 9), ErrRecovery, true
		}
		if current.PolicyDigest == digest {
			mode, _, modeErr := s.deps.Environment.ResolveMode(requestedMode)
			if modeErr != nil {
				return s.result("proxy on", "unsupported", "active", 7), modeErr, true
			}
			session, sessionErr := s.deps.Environment.SessionIdentity(ctx)
			if sessionErr != nil {
				return s.result("proxy on", "unsupported", "active", 7), sessionErr, true
			}
			if current.Mode != mode || current.ListenerProof.Mode != mode || current.ListenerProof.SessionIdentity != session {
				return s.result("proxy on", "conflict", "active", 6), ErrDifferentScope, true
			}
			result := s.result("proxy on", "idempotent", "active", 0)
			result.ActivationID, result.Generation, result.PolicyDigest, result.Mode = current.ActivationID, current.Generation, digest, current.Mode
			return result, nil, true
		}
		return s.result("proxy on", "conflict", "active", 6), ErrDifferentPolicy, true
	}
	return Result{}, nil, false
}

func (s *Service) Run(ctx context.Context, policyPath string, argv []string) (result Result, err error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if len(argv) == 0 || argv[0] == "" {
		return s.result("proxy run", "failed", "inactive", 2), errors.New("proxy run requires an exact command argv")
	}
	policy, digest, err := s.deps.Policies.Load(policyPath)
	if err != nil {
		return s.result("proxy run", "failed", "inactive", 2), err
	}
	result, err = s.activate(ctx, "proxy run", policy, digest, "scoped_run", "process_environment")
	if err != nil {
		return result, err
	}
	lease, leaseErr := s.deps.Store.AcquireScope(ctx, result.ActivationID, result.Generation)
	if leaseErr != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		cleanup, cleanupErr := s.recoverExpected(cleanupCtx, "proxy run", result.ActivationID, result.Generation)
		result.State, result.Cleanup, result.ManualRemediation = cleanup.State, cleanup.Cleanup, cleanup.ManualRemediation
		result.Status, result.ExitCode = "recovery_required", 9
		return result, errors.Join(ErrRecovery, leaseErr, cleanupErr)
	}
	runCtx, runCancel := context.WithCancel(ctx)
	leaseDone, leaseFailure := make(chan struct{}), make(chan error, 1)
	go func() {
		defer close(leaseDone)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if renewErr := s.deps.Store.RenewScope(runCtx, lease); renewErr != nil {
					leaseFailure <- renewErr
					runCancel()
					return
				}
			}
		}
	}()
	defer func() {
		runCancel()
		<-leaseDone
		recovered := recover()
		recoveryCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		releaseErr := s.deps.Store.ReleaseScope(recoveryCtx, lease)
		cleanup, cleanupErr := s.recoverExpected(recoveryCtx, "proxy run", result.ActivationID, result.Generation)
		result.State, result.Cleanup, result.ManualRemediation = cleanup.State, cleanup.Cleanup, cleanup.ManualRemediation
		if recovered != nil {
			err = errors.New("proxy runner panicked")
			result.Status, result.ExitCode = "failed", 1
		}
		select {
		case renewErr := <-leaseFailure:
			err = errors.Join(err, renewErr)
		default:
		}
		err = errors.Join(err, releaseErr, cleanupErr)
		if cleanupErr != nil {
			result.Status, result.ExitCode = "recovery_required", 9
		} else if err != nil {
			result.Status = "failed"
		} else {
			result.Status = "completed"
		}
	}()
	environment, present := s.activationEnvironment(result.ActivationID)
	if !present {
		result.Status, result.State, result.ExitCode = "recovery_required", "recovery_required", 9
		return result, ErrRecovery
	}
	childExit, runErr := s.deps.Runner.Run(runCtx, append([]string(nil), argv...), environment)
	result.ExitCode = childExit
	copy := childExit
	result.ChildExitCode = &copy
	err = runErr
	return result, err
}

func (s *Service) activate(ctx context.Context, command string, policy proxycontract.Policy, digest, mode, mechanism string) (Result, error) {
	activationID, nonce, err := randomIdentity()
	if err != nil {
		return s.result(command, "failed", "inactive", 1), err
	}
	session, err := s.deps.Environment.SessionIdentity(ctx)
	if err != nil {
		return s.result(command, "failed", "inactive", 7), err
	}
	metadata := ListenerMetadata{ActivationID: activationID, Nonce: nonce, Mode: mode, SessionIdentity: session, BinaryDigest: s.deps.Binary.Digest, Generation: 1, OwnerUID: int64(s.deps.OwnerUID)}
	managed, err := s.deps.Listeners.Start(ctx, policy, digest, metadata)
	if err != nil {
		return s.result(command, "failed", "inactive", 3), err
	}
	keep := false
	defer func() {
		if !keep {
			_ = shutdownVerified(context.Background(), managed)
		}
	}()
	identity := managed.Identity()
	expectedProof := listenerProof(identity, metadata)
	observedProof, live, inspectErr := managed.Inspect(ctx)
	if inspectErr != nil || !live || observedProof != expectedProof {
		return s.result(command, "failed", "inactive", 7), errors.Join(errors.New("listener identity could not be verified before publication"), inspectErr)
	}
	desiredEnvironment, err := managed.ChildEnvironment(s.deps.Environment.BaseEnvironment())
	if err != nil {
		return s.result(command, "failed", "inactive", 3), err
	}
	desired, err := selectedEnvironment(desiredEnvironment, activationID)
	if err != nil {
		return s.result(command, "failed", "inactive", 1), err
	}
	prior, err := s.deps.Environment.Snapshot(ctx, state.EnvironmentNames[:])
	if err != nil {
		return s.result(command, "failed", "inactive", 7), err
	}
	now := s.deps.Now().UTC()
	journal := &state.Journal{SchemaVersion: state.SchemaVersion, ActivationID: activationID, Nonce: nonce, Generation: 1, State: "prepared", OwnerUID: s.deps.OwnerUID, Platform: s.deps.Environment.Platform(), Mode: mode, SessionIdentity: session, Policy: state.PolicyIdentity{ID: policy.ID, Version: int64(policy.Version), Digest: digest}, Binary: s.deps.Binary, Listener: identity, CA: nil, CreatedAt: now, UpdatedAt: now}
	journal.Environment = journalEnvironment(prior, desired)
	journal.RollbackActions = rollbackActions(mode)
	publish := func() error { return s.deps.Environment.Publish(ctx, mechanism, desired) }
	if err := s.deps.Store.Activate(ctx, journal, mechanism, publish); err != nil {
		return s.result(command, "recovery_required", "recovery_required", 9), errors.Join(ErrRecovery, err)
	}
	s.mu.Lock()
	delete(s.stopped, identity.PID)
	s.listeners[identity.PID] = managedProof{listener: managed, activationID: activationID, proof: expectedProof}
	s.environments[activationID] = activationEnvironment{generation: 1, values: append([]string(nil), desiredEnvironment...)}
	s.mu.Unlock()
	keep = true
	result := s.result(command, "active", "active", 0)
	result.ActivationID, result.Generation, result.PolicyDigest, result.Mode, result.ListenerAddress = activationID, 1, digest, mode, identity.Address
	result.PublishedVariables = append([]string(nil), state.EnvironmentNames[:]...)
	verificationCtx, verificationCancel := boundedContext(ctx)
	observedProof, live, postErr := managed.Inspect(verificationCtx)
	current, reconcileErr := s.deps.Store.Reconcile(verificationCtx)
	verificationCancel()
	storeMatches := reconcileErr == nil && current.State == state.ReconciliationActive && current.ActivationID == activationID && current.Generation == 1 && current.PolicyDigest == digest && current.Mode == mode && current.ListenerProof != nil && *current.ListenerProof == expectedProof
	if postErr != nil || !live || observedProof != expectedProof || !storeMatches {
		recoveryCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if !storeMatches && reconcileErr == nil {
			reconcileErr = state.ErrLifecycleConflict
		}
		verificationErr := errors.Join(postErr, reconcileErr)
		if live {
			if shutdownErr := shutdownVerified(recoveryCtx, managed); shutdownErr != nil {
				result.Status, result.State, result.ExitCode = "recovery_required", "recovery_required", 9
				return result, errors.Join(ErrRecovery, verificationErr, shutdownErr)
			}
		}
		recovery, recoveryErr := s.recoverExpected(recoveryCtx, command, activationID, 1)
		if recoveryErr != nil {
			return recovery, errors.Join(ErrRecovery, verificationErr, recoveryErr)
		}
		recovery.Status, recovery.ExitCode = "failed", 7
		return recovery, errors.Join(errors.New("listener or activation authority changed after publication"), verificationErr)
	}
	return result, nil
}

func (s *Service) activationEnvironment(id string) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.environments[id]
	return append([]string(nil), value.values...), ok
}

func (s *Service) Off(ctx context.Context, removeCA bool) (Result, error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if removeCA {
		return s.result("proxy off", "unsupported", "unknown", 7), ErrCARemovalUnsupported
	}
	return s.recover(ctx, "proxy off")
}
func (s *Service) Reset(ctx context.Context, yes, removeCA bool) (Result, error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if !yes {
		return s.result("proxy reset", "confirmation_required", "unknown", 2), errors.New("proxy reset requires --yes")
	}
	if removeCA {
		return s.result("proxy reset", "unsupported", "unknown", 7), ErrCARemovalUnsupported
	}
	return s.recover(ctx, "proxy reset")
}
func (s *Service) recoverExpected(ctx context.Context, command, id string, generation int64) (Result, error) {
	return s.recoverWith(ctx, command, id, generation, func(callCtx context.Context) (state.RecoveryResult, error) {
		return s.deps.Store.RecoverExact(callCtx, s.deps.Environment, s, state.ExpectedRecovery{ActivationID: id, Generation: generation})
	})
}
func (s *Service) recover(ctx context.Context, command string) (result Result, err error) {
	bounded, cancel := boundedContext(ctx)
	defer cancel()
	current, reconcileErr := s.deps.Store.Reconcile(bounded)
	if current.ActivationID != "" && current.Generation > 0 {
		return s.recoverExpected(bounded, command, current.ActivationID, current.Generation)
	}
	if reconcileErr == nil && current.State == state.ReconciliationInactive {
		return s.recoverWith(bounded, command, "", 0, func(context.Context) (state.RecoveryResult, error) {
			return state.RecoveryResult{Status: state.RecoveryNotActive}, nil
		})
	}
	return s.recoverWith(bounded, command, "", 0, func(context.Context) (state.RecoveryResult, error) {
		return state.RecoveryResult{Status: state.RecoveryRequired}, errors.Join(state.ErrRecoveryRequired, reconcileErr)
	})
}
func (s *Service) recoverWith(ctx context.Context, command, activationID string, generation int64, operation func(context.Context) (state.RecoveryResult, error)) (result Result, err error) {
	defer func() {
		if recover() != nil {
			result = s.result(command, "recovery_required", "recovery_required", 9)
			err = ErrRecovery
		}
	}()
	bounded, cancel := boundedContext(ctx)
	defer cancel()
	recovery, err := operation(bounded)
	if err == nil && recovery.Status == state.RecoveryComplete && activationID != "" && generation > 0 {
		s.purgeActivation(activationID, generation)
	}
	stateValue, status, exit := "inactive", "inactive", 0
	if err != nil || recovery.Status == state.RecoveryRequired || errors.Is(err, state.ErrRecoveryRequired) {
		stateValue, status, exit = "recovery_required", "recovery_required", 9
		if !errors.Is(err, ErrRecovery) {
			err = errors.Join(ErrRecovery, err)
		}
	}
	result = s.result(command, status, stateValue, exit)
	cleanupStatus := string(recovery.Status)
	if exit == 9 {
		cleanupStatus = "recovery_required"
	}
	if cleanupStatus == "" {
		cleanupStatus = status
	}
	result.Cleanup = &CleanupReceipt{Status: cleanupStatus, ListenerEvidence: recovery.ListenerEvidence, RestoredVariables: append([]string(nil), recovery.RestoredEnvironment...), ConflictedVariables: append([]string(nil), recovery.ConflictedEnvironment...)}
	result.ManualRemediation = append([]string(nil), recovery.ManualRemediation...)
	return result, err
}

func (s *Service) purgeActivation(activationID string, generation int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if environment, present := s.environments[activationID]; present && environment.generation == generation {
		delete(s.environments, activationID)
	}
	for pid, managed := range s.listeners {
		if managed.activationID == activationID && managed.proof.Generation == generation {
			delete(s.listeners, pid)
		}
	}
	for pid, stopped := range s.stopped {
		if stopped.activationID == activationID && stopped.proof.Generation == generation {
			delete(s.stopped, pid)
		}
	}
}

func (s *Service) Inspect(ctx context.Context, pid int) (state.LiveListenerProof, bool, error) {
	s.mu.Lock()
	managed, ok := s.listeners[pid]
	_, stopped := s.stopped[pid]
	s.mu.Unlock()
	if ok {
		proof, live, err := managed.listener.Inspect(ctx)
		if err != nil || !live {
			return state.LiveListenerProof{}, live, err
		}
		return proof, true, nil
	}
	if stopped {
		return state.LiveListenerProof{}, false, nil
	}
	return s.deps.Controller.Inspect(ctx, pid)
}
func (s *Service) Stop(ctx context.Context, proof state.LiveListenerProof) error {
	s.mu.Lock()
	managed, ok := s.listeners[proof.PID]
	s.mu.Unlock()
	if ok {
		if managed.proof != proof {
			return errors.New("managed listener identity mismatch")
		}
		if err := shutdownVerified(ctx, managed.listener); err != nil {
			return err
		}
		s.mu.Lock()
		if current, present := s.listeners[proof.PID]; present && current.proof == proof {
			delete(s.listeners, proof.PID)
			if environment, exists := s.environments[current.activationID]; exists && environment.generation == proof.Generation {
				delete(s.environments, current.activationID)
			}
			s.stopped[proof.PID] = stoppedProof{activationID: current.activationID, proof: proof}
		}
		s.mu.Unlock()
		return nil
	}
	return s.deps.Controller.Stop(ctx, proof)
}
func (s *Service) Status(ctx context.Context) (Result, error) {
	value, err := s.deps.Store.Reconcile(ctx)
	result := s.result("proxy status", string(value.State), string(value.State), 0)
	result.ActivationID, result.PolicyDigest, result.Mode = value.ActivationID, value.PolicyDigest, value.Mode
	if err != nil {
		result.Status, result.State = "recovery_required", "recovery_required"
		result.ExitCode = 9
		if !errors.Is(err, ErrRecovery) {
			err = errors.Join(ErrRecovery, err)
		}
	}
	if value.State == state.ReconciliationActive {
		if value.ListenerProof == nil {
			result.Status, result.State, result.ExitCode = "recovery_required", "recovery_required", 9
			return result, ErrRecovery
		}
		observed, live, inspectErr := s.Inspect(ctx, value.ListenerProof.PID)
		if inspectErr != nil || !live || observed != *value.ListenerProof {
			result.Status, result.State, result.ExitCode = "recovery_required", "recovery_required", 9
			return result, errors.Join(ErrRecovery, inspectErr)
		}
	}
	return result, err
}
func (s *Service) Doctor(ctx context.Context, policyPath string) (Result, error) {
	policy, digest, err := s.deps.Policies.Load(policyPath)
	if err != nil {
		return s.result("proxy doctor", "failed", "inactive", 2), err
	}
	mode, _, err := s.deps.Environment.ResolveMode("auto")
	if err != nil {
		if errors.Is(err, ErrSessionUnsupported) {
			return s.result("proxy doctor", "warning", "inactive", 0), nil
		}
		return s.result("proxy doctor", "failed", "inactive", 7), err
	}
	session, err := s.deps.Environment.SessionIdentity(ctx)
	if err != nil {
		return s.result("proxy doctor", "failed", "inactive", 7), err
	}
	id, nonce, randomErr := randomIdentity()
	if randomErr != nil {
		return s.result("proxy doctor", "failed", "inactive", 1), randomErr
	}
	metadata := ListenerMetadata{ActivationID: id, Nonce: nonce, Mode: mode, SessionIdentity: session, BinaryDigest: s.deps.Binary.Digest, Generation: 1, OwnerUID: int64(s.deps.OwnerUID)}
	managed, err := s.deps.Listeners.Start(ctx, policy, digest, metadata)
	if err != nil {
		return s.result("proxy doctor", "failed", "inactive", 3), err
	}
	identity := managed.Identity()
	observedProof, liveBefore, inspectBeforeErr := managed.Inspect(ctx)
	if inspectBeforeErr != nil || !liveBefore || observedProof != listenerProof(identity, metadata) {
		shutdownErr := shutdownVerified(context.Background(), managed)
		return s.result("proxy doctor", "failed", "inactive", 7), errors.Join(errors.New("proxy doctor listener identity or liveness is invalid"), inspectBeforeErr, shutdownErr)
	}
	if shutdownErr := shutdownVerified(ctx, managed); shutdownErr != nil {
		return s.result("proxy doctor", "failed", "inactive", 7), shutdownErr
	}
	if _, live, inspectErr := managed.Inspect(ctx); inspectErr != nil || live {
		return s.result("proxy doctor", "failed", "inactive", 7), errors.Join(errors.New("proxy doctor listener shutdown is unverified"), inspectErr)
	}
	result := s.result("proxy doctor", "ready", "inactive", 0)
	result.PolicyDigest, result.ListenerAddress = digest, identity.Address
	return result, nil
}
func boundedContext(parent context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := parent.Deadline(); ok && time.Until(deadline) <= cleanupTimeout {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, cleanupTimeout)
}
func shutdownVerified(ctx context.Context, managed ManagedListener) error {
	bounded, cancel := boundedContext(ctx)
	defer cancel()
	shutdownErr := managed.Shutdown(bounded)
	inspectCtx, inspectCancel := context.WithTimeout(context.Background(), time.Second)
	defer inspectCancel()
	_, live, inspectErr := managed.Inspect(inspectCtx)
	if inspectErr == nil && !live {
		return nil
	}
	return errors.Join(shutdownErr, inspectErr, func() error {
		if live {
			return errors.New("listener remains live after bounded shutdown")
		}
		return nil
	}())
}
func (s *Service) Routes(_ context.Context, policyPath string) ([]Route, error) {
	policy, _, err := s.deps.Policies.Load(policyPath)
	if err != nil {
		return nil, err
	}
	values := make([]Route, 0, len(policy.Routes))
	for _, route := range policy.Routes {
		values = append(values, Route{string(route.ID), string(route.DestinationClass), string(route.DestinationProtocol), fmt.Sprintf("%s://%s:%d%s", route.Endpoint.Scheme, route.Endpoint.Hostname, route.Endpoint.Port, route.Endpoint.BasePath)})
	}
	sort.Slice(values, func(i, j int) bool { return values[i].ID < values[j].ID })
	return values, nil
}
func (s *Service) Tail(ctx context.Context, cursor string, follow bool) ([]Event, error) {
	if s.deps.Events == nil {
		return []Event{}, nil
	}
	events, err := s.deps.Events.Read(ctx, cursor, follow)
	if err != nil {
		return nil, err
	}
	if len(events) > 1000 {
		return nil, errors.New("proxy event batch exceeds limit")
	}
	for _, event := range events {
		if err := event.Validate(); err != nil {
			return nil, errors.New("proxy event is outside the redacted contract")
		}
	}
	return events, nil
}
func (s *Service) result(command, status, stateValue string, exit int) Result {
	return Result{Command: command, ContractVersion: ContractVersion, Status: status, State: stateValue, Timestamp: s.deps.Now().UTC().Format(time.RFC3339), ExitCode: exit}
}

func listenerProof(identity state.ListenerIdentity, metadata ListenerMetadata) state.LiveListenerProof {
	return state.LiveListenerProof{
		PID: identity.PID, ProcessStartIdentity: identity.ProcessStartIdentity, ExecutableIdentity: identity.ExecutableIdentity,
		BinaryDigest: metadata.BinaryDigest, ListenerKeyFingerprint: identity.ListenerKeyFingerprint, ActivationNonce: metadata.Nonce,
		OwnerUID: int(metadata.OwnerUID), Generation: metadata.Generation, Mode: metadata.Mode, SessionIdentity: metadata.SessionIdentity,
	}
}

func randomIdentity() (string, string, error) {
	var identity [16]byte
	var nonce [24]byte
	if _, err := rand.Read(identity[:]); err != nil {
		return "", "", err
	}
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", "", err
	}
	identity[6] = (identity[6] & 0x0f) | 0x40
	identity[8] = (identity[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(identity[:])
	id := encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
	return id, base64.RawURLEncoding.EncodeToString(nonce[:]), nil
}
func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}
func selectedEnvironment(values []string, marker string) ([]PublishedValue, error) {
	found := map[string]string{}
	for _, entry := range values {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			found[name] = value
		}
	}
	result := make([]PublishedValue, 0, 5)
	for _, name := range state.EnvironmentNames {
		value, ok := found[name]
		if !ok {
			return nil, errors.New("listener environment is incomplete")
		}
		result = append(result, PublishedValue{Name: name, Value: value, Marker: marker + ":" + name})
	}
	return result, nil
}
func journalEnvironment(prior map[string]PriorValue, desired []PublishedValue) []state.JournalEnvironment {
	result := make([]state.JournalEnvironment, 0, 5)
	for _, item := range desired {
		before := prior[item.Name]
		var priorValue *string
		action := "remove_blazn_value"
		if before.Present {
			copy := before.Value
			priorValue = &copy
			action = "restore_prior_value"
		}
		result = append(result, state.JournalEnvironment{Name: item.Name, PriorPresent: before.Present, PriorValue: priorValue, DesiredValueDigest: digest(item.Value), ActivationMarker: item.Marker, RollbackAction: action})
	}
	return result
}
func rollbackActions(mode string) []state.RollbackAction {
	values := []state.RollbackAction{{Ordinal: 1, Operation: "restore_environment", Target: "documented_proxy_variables"}, {Ordinal: 2, Operation: "stop_listener", Target: "exact_listener_identity"}}
	if mode == "scoped_run" {
		values = append(values, state.RollbackAction{Ordinal: 3, Operation: "remove_scoped_state", Target: "activation_records"})
	}
	return values
}

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

var (
	ErrUnavailable     = errors.New("proxy platform adapter is unavailable")
	ErrDifferentPolicy = errors.New("proxy already active with a different policy")
	ErrRecovery        = state.ErrRecoveryRequired
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
	ChildEnvironment([]string) ([]string, error)
	Shutdown(context.Context) error
}

type ListenerFactory interface {
	Start(context.Context, proxycontract.Policy, string, ListenerMetadata) (ManagedListener, error)
}

type ListenerMetadata struct {
	ActivationID, Nonce, Mode, SessionIdentity string
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
}

type PersistentStore struct{ Value *state.Store }

func (p PersistentStore) Reconcile(ctx context.Context) (state.Reconciliation, error) {
	return p.Value.Reconcile(ctx)
}
func (p PersistentStore) Recover(ctx context.Context, environment state.EnvironmentRestorer, listener state.ListenerController) (state.RecoveryResult, error) {
	return p.Value.Recover(ctx, environment, listener)
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
	deps      Dependencies
	mu        sync.Mutex
	listeners map[int]managedProof
}
type managedProof struct {
	listener ManagedListener
	proof    state.LiveListenerProof
}

type Result struct {
	Command            string   `json:"command"`
	ContractVersion    string   `json:"contractVersion"`
	Status             string   `json:"status"`
	State              string   `json:"state"`
	Timestamp          string   `json:"timestamp"`
	ActivationID       string   `json:"activationId,omitempty"`
	PolicyDigest       string   `json:"policyDigest,omitempty"`
	Mode               string   `json:"mode,omitempty"`
	ListenerAddress    string   `json:"listenerAddress,omitempty"`
	PublishedVariables []string `json:"publishedVariables,omitempty"`
	ManualRemediation  []string `json:"manualRemediation,omitempty"`
	ExitCode           int      `json:"exitCode"`
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
	return &Service{deps: deps, listeners: map[int]managedProof{}}, nil
}

func (s *Service) On(ctx context.Context, policyPath, requestedMode string) (Result, error) {
	policy, digest, err := s.deps.Policies.Load(policyPath)
	if err != nil {
		return s.result("proxy on", "failed", "inactive", 2), err
	}
	current, reconcileErr := s.deps.Store.Reconcile(ctx)
	if current.State == state.ReconciliationRecoveryRequired || errors.Is(reconcileErr, state.ErrRecoveryRequired) {
		return s.result("proxy on", "recovery_required", "recovery_required", 9), ErrRecovery
	}
	if reconcileErr != nil {
		return s.result("proxy on", "recovery_required", "recovery_required", 9), errors.Join(ErrRecovery, reconcileErr)
	}
	if current.State == state.ReconciliationActive {
		if current.ListenerProof == nil {
			return s.result("proxy on", "recovery_required", "recovery_required", 9), ErrRecovery
		}
		observed, live, inspectErr := s.Inspect(ctx, current.ListenerProof.PID)
		if inspectErr != nil || !live || observed != *current.ListenerProof {
			return s.result("proxy on", "recovery_required", "recovery_required", 9), ErrRecovery
		}
		if current.PolicyDigest == digest {
			result := s.result("proxy on", "idempotent", "active", 0)
			result.ActivationID, result.PolicyDigest, result.Mode = current.ActivationID, digest, current.Mode
			return result, nil
		}
		return s.result("proxy on", "conflict", "active", 6), ErrDifferentPolicy
	}
	mode, mechanism, err := s.deps.Environment.ResolveMode(requestedMode)
	if err != nil {
		return s.result("proxy on", "unsupported", "inactive", 7), err
	}
	return s.activate(ctx, "proxy on", policy, digest, mode, mechanism, nil)
}

func (s *Service) Run(ctx context.Context, policyPath string, argv []string) (Result, error) {
	if len(argv) == 0 || argv[0] == "" {
		return s.result("proxy run", "failed", "inactive", 2), errors.New("proxy run requires an exact command argv")
	}
	policy, digest, err := s.deps.Policies.Load(policyPath)
	if err != nil {
		return s.result("proxy run", "failed", "inactive", 2), err
	}
	childExit, ran := 0, false
	result, err := s.activate(ctx, "proxy run", policy, digest, "scoped_run", "process_environment", func(environment []string) error {
		var runErr error
		ran = true
		childExit, runErr = s.deps.Runner.Run(ctx, append([]string(nil), argv...), environment)
		return runErr
	})
	if ran {
		result.ExitCode = childExit
	}
	if result.ActivationID != "" {
		recovery, offErr := s.recover(ctx, "proxy run")
		result.State = recovery.State
		result.ManualRemediation = recovery.ManualRemediation
		err = errors.Join(err, offErr)
	}
	return result, err
}

func (s *Service) activate(ctx context.Context, command string, policy proxycontract.Policy, digest, mode, mechanism string, run func([]string) error) (Result, error) {
	activationID, nonce, err := randomIdentity()
	if err != nil {
		return s.result(command, "failed", "inactive", 1), err
	}
	session, err := s.deps.Environment.SessionIdentity(ctx)
	if err != nil {
		return s.result(command, "failed", "inactive", 7), err
	}
	metadata := ListenerMetadata{ActivationID: activationID, Nonce: nonce, Mode: mode, SessionIdentity: session, Generation: 1, OwnerUID: int64(s.deps.OwnerUID)}
	managed, err := s.deps.Listeners.Start(ctx, policy, digest, metadata)
	if err != nil {
		return s.result(command, "failed", "inactive", 3), err
	}
	keep := false
	defer func() {
		if !keep {
			_ = managed.Shutdown(context.Background())
		}
	}()
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
	journal := &state.Journal{SchemaVersion: state.SchemaVersion, ActivationID: activationID, Nonce: nonce, Generation: 1, State: "prepared", OwnerUID: s.deps.OwnerUID, Platform: s.deps.Environment.Platform(), Mode: mode, SessionIdentity: session, Policy: state.PolicyIdentity{ID: policy.ID, Version: int64(policy.Version), Digest: digest}, Binary: s.deps.Binary, Listener: managed.Identity(), CA: nil, CreatedAt: now, UpdatedAt: now}
	journal.Environment = journalEnvironment(prior, desired)
	journal.RollbackActions = rollbackActions(mode)
	publish := func() error { return s.deps.Environment.Publish(ctx, mechanism, desired) }
	if err := s.deps.Store.Activate(ctx, journal, mechanism, publish); err != nil {
		return s.result(command, "recovery_required", "recovery_required", 9), errors.Join(ErrRecovery, err)
	}
	s.mu.Lock()
	s.listeners[managed.Identity().PID] = managedProof{listener: managed, proof: state.LiveListenerProof{PID: managed.Identity().PID, ProcessStartIdentity: managed.Identity().ProcessStartIdentity, ExecutableIdentity: managed.Identity().ExecutableIdentity, BinaryDigest: s.deps.Binary.Digest, ListenerKeyFingerprint: managed.Identity().ListenerKeyFingerprint, ActivationNonce: nonce, OwnerUID: s.deps.OwnerUID, Generation: 1, Mode: mode, SessionIdentity: session}}
	s.mu.Unlock()
	keep = true
	result := s.result(command, "active", "active", 0)
	result.ActivationID, result.PolicyDigest, result.Mode, result.ListenerAddress = activationID, digest, mode, managed.Identity().Address
	result.PublishedVariables = append([]string(nil), state.EnvironmentNames[:]...)
	if run != nil {
		err = run(desiredEnvironment)
	}
	return result, err
}

func (s *Service) Off(ctx context.Context) (Result, error) { return s.recover(ctx, "proxy off") }
func (s *Service) Reset(ctx context.Context, yes, _ bool) (Result, error) {
	if !yes {
		return s.result("proxy reset", "confirmation_required", "unknown", 2), errors.New("proxy reset requires --yes")
	}
	return s.recover(ctx, "proxy reset")
}
func (s *Service) recover(ctx context.Context, command string) (result Result, err error) {
	defer func() {
		if recover() != nil {
			result = s.result(command, "recovery_required", "recovery_required", 9)
			err = ErrRecovery
		}
	}()
	recovery, err := s.deps.Store.Recover(ctx, s.deps.Environment, s)
	stateValue, status, exit := "inactive", "inactive", 0
	if err != nil || recovery.Status == state.RecoveryRequired || errors.Is(err, state.ErrRecoveryRequired) {
		stateValue, status, exit = "recovery_required", "recovery_required", 9
		if !errors.Is(err, ErrRecovery) {
			err = errors.Join(ErrRecovery, err)
		}
	}
	result = s.result(command, status, stateValue, exit)
	result.ManualRemediation = append([]string(nil), recovery.ManualRemediation...)
	return result, err
}

func (s *Service) Inspect(ctx context.Context, pid int) (state.LiveListenerProof, bool, error) {
	s.mu.Lock()
	managed, ok := s.listeners[pid]
	s.mu.Unlock()
	if ok {
		return managed.proof, true, nil
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
		if err := managed.listener.Shutdown(ctx); err != nil {
			return err
		}
		s.mu.Lock()
		if current, present := s.listeners[proof.PID]; present && current.proof == proof {
			delete(s.listeners, proof.PID)
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
	return result, err
}
func (s *Service) Doctor(ctx context.Context, policyPath string) (Result, error) {
	policy, digest, err := s.deps.Policies.Load(policyPath)
	if err != nil {
		return s.result("proxy doctor", "failed", "inactive", 2), err
	}
	mode, _, err := s.deps.Environment.ResolveMode("auto")
	if err != nil {
		return s.result("proxy doctor", "warning", "inactive", 0), nil
	}
	session, err := s.deps.Environment.SessionIdentity(ctx)
	if err != nil {
		return s.result("proxy doctor", "failed", "inactive", 7), err
	}
	id, nonce, randomErr := randomIdentity()
	if randomErr != nil {
		return s.result("proxy doctor", "failed", "inactive", 1), randomErr
	}
	managed, err := s.deps.Listeners.Start(ctx, policy, digest, ListenerMetadata{ActivationID: id, Nonce: nonce, Mode: mode, SessionIdentity: session, Generation: 1, OwnerUID: int64(s.deps.OwnerUID)})
	if err != nil {
		return s.result("proxy doctor", "failed", "inactive", 3), err
	}
	defer managed.Shutdown(context.Background())
	result := s.result("proxy doctor", "ready", "inactive", 0)
	result.PolicyDigest, result.ListenerAddress = digest, managed.Identity().Address
	return result, nil
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

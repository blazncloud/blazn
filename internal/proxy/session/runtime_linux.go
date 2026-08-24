//go:build linux

// Package session wires the capability-gated Linux durable listener to the
// activation core. Construction is side-effect free; policy, destination
// credentials, systemd capability, session identity, and binary identity are
// all proven before the protected state root or listener is created.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/blazncloud/blazn/internal/proxy/activation"
	"github.com/blazncloud/blazn/internal/proxy/credential"
	"github.com/blazncloud/blazn/internal/proxy/process"
	"github.com/blazncloud/blazn/internal/proxy/router"
	"github.com/blazncloud/blazn/internal/proxy/state"
	"github.com/blazncloud/blazn/internal/proxy/systemdenv"
	"github.com/blazncloud/blazn/internal/proxycontract"
)

type scopedCommands interface {
	Run(context.Context, string, []string) (activation.Result, error)
}

// Commands combines durable Linux session commands with the existing scoped
// runner. It retains restart material only in memory; cross-process recovery
// reconstructs the listener token from the validated exact-five systemd
// environment and derives the owner-only control socket from activation state.
type Commands struct {
	scoped   scopedCommands
	load     activation.PolicyLoader
	resolve  credential.SnapshotResolver
	env      activation.Environment
	platform *process.UnixPlatform
	now      func() time.Time
}

// NewProduction performs no filesystem, D-Bus, process, proxy, CA, or config
// mutation. Every operation constructs its native dependencies lazily.
func NewProduction(scoped scopedCommands) (*Commands, error) {
	if scoped == nil {
		return nil, activation.ErrUnavailable
	}
	backend := credential.NewSecretServiceBackend()
	return &Commands{
		scoped:  scoped,
		load:    activation.PolicyLoaderFunc(router.LoadPolicy),
		resolve: &credential.Resolver{NodeRoute: backend, WorkspaceVault: backend},
		env:     systemdenv.New(), platform: process.NewUnixPlatform(), now: time.Now,
	}, nil
}

func (c *Commands) On(ctx context.Context, policyPath, requestedMode string) (activation.Result, error) {
	policy, digest, snapshot, err := c.preflight(ctx, policyPath, requestedMode)
	if err != nil {
		return c.result("proxy on", "unsupported", "inactive", modeExit(err)), err
	}
	defer snapshot.Destroy()
	service, _, err := c.service(ctx, frozenPolicy{path: filepath.Clean(policyPath), policy: policy, digest: digest}, snapshot)
	if err != nil {
		return c.result("proxy on", "failed", "inactive", 7), err
	}
	if err := prepareRestart(ctx, service); err != nil && !errors.Is(err, state.ErrNotFound) {
		return c.result("proxy on", "recovery_required", "recovery_required", 9), errors.Join(activation.ErrRecovery, err)
	}
	result, onErr := service.value.On(ctx, policyPath, requestedMode)
	if onErr == nil || result.State != "recovery_required" {
		return result, onErr
	}
	// Publication is journaled before the first systemd write. If any write or
	// post-publication verification fails, immediately attempt exact recovery
	// with the still-authenticated controller and pinned manager proof.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cleanup, cleanupErr := service.value.Off(cleanupCtx, false)
	if cleanupErr != nil {
		return result, errors.Join(onErr, cleanupErr)
	}
	result.Status, result.State, result.ExitCode = "failed", "inactive", 7
	result.Cleanup = cleanup.Cleanup
	return result, activation.ErrUnavailable
}

func (c *Commands) Doctor(ctx context.Context, policyPath string) (activation.Result, error) {
	policy, digest, snapshot, err := c.preflight(ctx, policyPath, "auto")
	if err != nil {
		if errors.Is(err, activation.ErrSessionUnsupported) {
			return c.result("proxy doctor", "warning", "inactive", 0), nil
		}
		return c.result("proxy doctor", "failed", "inactive", modeExit(err)), err
	}
	defer snapshot.Destroy()
	service, _, err := c.service(ctx, frozenPolicy{path: filepath.Clean(policyPath), policy: policy, digest: digest}, snapshot)
	if err != nil {
		return c.result("proxy doctor", "failed", "inactive", 7), err
	}
	return service.value.Doctor(ctx, policyPath)
}

func (c *Commands) Off(ctx context.Context, removeCA bool) (activation.Result, error) {
	service, _, err := c.service(ctx, unavailablePolicy{}, nil)
	if err != nil {
		return c.result("proxy off", "failed", "unknown", 7), err
	}
	if err := prepareRestart(ctx, service); err != nil && !errors.Is(err, state.ErrNotFound) {
		return c.result("proxy off", "recovery_required", "recovery_required", 9), errors.Join(activation.ErrRecovery, err)
	}
	return service.value.Off(ctx, removeCA)
}

func (c *Commands) Reset(ctx context.Context, yes, removeCA bool) (activation.Result, error) {
	service, _, err := c.service(ctx, unavailablePolicy{}, nil)
	if err != nil {
		return c.result("proxy reset", "failed", "unknown", 7), err
	}
	if err := prepareRestart(ctx, service); err != nil && !errors.Is(err, state.ErrNotFound) {
		return c.result("proxy reset", "recovery_required", "recovery_required", 9), errors.Join(activation.ErrRecovery, err)
	}
	return service.value.Reset(ctx, yes, removeCA)
}

func (c *Commands) Status(ctx context.Context) (activation.Result, error) {
	service, _, err := c.service(ctx, unavailablePolicy{}, nil)
	if err != nil {
		return c.result("proxy status", "failed", "unknown", 7), err
	}
	if err := prepareRestart(ctx, service); err != nil && !errors.Is(err, state.ErrNotFound) {
		return c.result("proxy status", "recovery_required", "recovery_required", 9), errors.Join(activation.ErrRecovery, err)
	}
	return service.value.Status(ctx)
}

func (c *Commands) Run(ctx context.Context, policyPath string, argv []string) (activation.Result, error) {
	return c.scoped.Run(ctx, policyPath, argv)
}

func (c *Commands) Routes(_ context.Context, policyPath string) ([]activation.Route, error) {
	policy, _, err := c.load.Load(policyPath)
	if err != nil {
		return nil, err
	}
	routes := make([]activation.Route, 0, len(policy.Routes))
	for _, route := range policy.Routes {
		routes = append(routes, activation.Route{ID: string(route.ID), DestinationClass: string(route.DestinationClass), DestinationProtocol: string(route.DestinationProtocol), Endpoint: fmt.Sprintf("%s://%s:%d%s", route.Endpoint.Scheme, route.Endpoint.Hostname, route.Endpoint.Port, route.Endpoint.BasePath)})
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].ID < routes[j].ID })
	return routes, nil
}

func (*Commands) Tail(context.Context, string, bool) ([]activation.Event, error) {
	return []activation.Event{}, nil
}

func (c *Commands) preflight(ctx context.Context, policyPath, requestedMode string) (proxycontract.Policy, string, *credential.Snapshot, error) {
	policy, digest, err := c.load.Load(policyPath)
	if err != nil {
		return proxycontract.Policy{}, "", nil, err
	}
	if requestedMode != "auto" && requestedMode != "session" {
		return proxycontract.Policy{}, "", nil, activation.ErrSessionUnsupported
	}
	mode, mechanism, err := c.env.ResolveMode(requestedMode)
	if err != nil || mode != "session" || mechanism != "systemd_user_environment" {
		return proxycontract.Policy{}, "", nil, errors.Join(activation.ErrSessionUnsupported, err)
	}
	if _, err := c.env.SessionIdentity(ctx); err != nil {
		return proxycontract.Policy{}, "", nil, err
	}
	if _, live, err := c.platform.Evidence(ctx, os.Getpid()); err != nil || !live {
		return proxycontract.Policy{}, "", nil, errors.Join(activation.ErrUnavailable, err)
	}
	snapshot, err := c.resolve.Resolve(ctx, policy)
	if err != nil {
		return proxycontract.Policy{}, "", nil, errors.Join(activation.ErrCredentialUnavailable, err)
	}
	return policy, digest, snapshot, nil
}

type serviceRuntime struct {
	value       *activation.Service
	store       *state.Store
	environment activation.Environment
	controller  *process.Controller
	binary      state.BinaryIdentity
}

func (c *Commands) service(ctx context.Context, policy activation.PolicyLoader, snapshot *credential.Snapshot) (*serviceRuntime, *processListenerFactory, error) {
	evidence, live, err := c.platform.Evidence(ctx, os.Getpid())
	if err != nil || !live {
		return nil, nil, errors.Join(activation.ErrUnavailable, err)
	}
	store, err := state.NewForCurrentAccount()
	if err != nil {
		return nil, nil, err
	}
	controller := &process.Controller{Platform: c.platform}
	factory := &processListenerFactory{controller: controller, binary: state.BinaryIdentity{Path: evidence.ExecutablePath, Digest: evidence.BinaryDigest}, snapshot: snapshot}
	value, err := activation.New(activation.Dependencies{
		Store: activation.PersistentStore{Value: store}, Environment: c.env, Listeners: factory,
		Controller: controller, Policies: policy, Runner: unavailableRunner{},
		Binary: factory.binary, OwnerUID: os.Getuid(), Now: c.now,
	})
	if err != nil {
		return nil, nil, err
	}
	return &serviceRuntime{value: value, store: store, environment: c.env, controller: controller, binary: factory.binary}, factory, nil
}

func prepareRestart(ctx context.Context, runtime *serviceRuntime) error {
	current, err := runtime.store.Reconcile(ctx)
	if err != nil && current.ListenerProof == nil {
		if current.State == state.ReconciliationInactive {
			return state.ErrNotFound
		}
		return err
	}
	if current.State == state.ReconciliationInactive || current.ListenerProof == nil {
		return state.ErrNotFound
	}
	if current.Mode != "session" || current.ListenerProof.OwnerUID != os.Getuid() || current.ListenerProof.BinaryDigest != runtime.binary.Digest {
		return process.ErrUnauthorized
	}
	if err := runtime.controller.Expect(*current.ListenerProof, runtime.binary.Path); err != nil {
		return err
	}
	_, processLive, evidenceErr := runtime.controller.Platform.Evidence(ctx, current.ListenerProof.PID)
	if evidenceErr != nil {
		return evidenceErr
	}
	if !processLive {
		// Recovery can safely observe absence again under the lifecycle lock and
		// restore exact publication without any control credential.
		return nil
	}
	values, snapshotErr := runtime.environment.Snapshot(ctx, state.EnvironmentNames[:])
	if snapshotErr != nil {
		return snapshotErr
	}
	token, err := recoveredToken(values, current.ListenerAddress)
	if err != nil {
		return err
	}
	control := filepath.Join(runtime.store.Paths().Root, "proxy-control-"+current.ActivationID+".sock")
	_, live, err := runtime.controller.Discover(ctx, process.PersistedListener{Proof: *current.ListenerProof, ControlAddress: control, ExecutablePath: runtime.binary.Path, ListenerToken: token})
	if err != nil {
		return errors.Join(process.ErrRestartMaterialUnavailable, err)
	}
	if !live {
		return nil
	}
	return nil
}

func recoveredToken(values map[string]activation.PriorValue, listenerAddress string) (string, error) {
	if listenerAddress == "" {
		return "", process.ErrRestartMaterialUnavailable
	}
	openAI, anthropic, auth := values["OPENAI_API_KEY"], values["ANTHROPIC_API_KEY"], values["ANTHROPIC_AUTH_TOKEN"]
	if !openAI.Present || !anthropic.Present || !auth.Present || openAI.Value == "" || openAI.Value != anthropic.Value || openAI.Value != auth.Value {
		return "", process.ErrRestartMaterialUnavailable
	}
	openAIURL, anthropicURL := values["OPENAI_BASE_URL"], values["ANTHROPIC_BASE_URL"]
	base := (&url.URL{Scheme: "http", Host: listenerAddress}).String()
	if !openAIURL.Present || !anthropicURL.Present || openAIURL.Value != base+"/v1" || anthropicURL.Value != base {
		return "", process.ErrRestartMaterialUnavailable
	}
	return openAI.Value, nil
}

type processListenerFactory struct {
	controller *process.Controller
	binary     state.BinaryIdentity
	snapshot   *credential.Snapshot
}

func (f *processListenerFactory) Start(ctx context.Context, policy proxycontract.Policy, digest string, metadata activation.ListenerMetadata) (activation.ManagedListener, error) {
	if f == nil || f.controller == nil || f.snapshot == nil {
		return nil, activation.ErrListenerUnavailable
	}
	actualDigest, err := proxycontract.ContractDigest(policy)
	if err != nil || actualDigest != digest {
		return nil, activation.ErrPolicyInvalid
	}
	policyBytes, err := json.Marshal(policy)
	if err != nil {
		return nil, activation.ErrPolicyInvalid
	}
	exported := f.snapshot.Export()
	credentials := make([]process.Credential, 0, len(exported))
	defer func() {
		for index := range exported {
			zero(exported[index].Value)
		}
		for index := range credentials {
			zero(credentials[index].Value)
		}
	}()
	for _, value := range exported {
		credentials = append(credentials, process.Credential{Reference: value.Reference, Value: append([]byte(nil), value.Value...)})
	}
	managed, err := f.controller.Start(ctx, process.StartRequest{Metadata: process.Metadata{
		ActivationID: metadata.ActivationID, Nonce: metadata.Nonce, Generation: metadata.Generation,
		OwnerUID: int(metadata.OwnerUID), Mode: metadata.Mode, SessionIdentity: metadata.SessionIdentity,
		BinaryPath: f.binary.Path, BinaryDigest: f.binary.Digest,
	}, Policy: policyBytes, Credentials: credentials})
	if err != nil {
		return nil, errors.Join(activation.ErrListenerUnavailable, err)
	}
	return &managedListener{managed: managed}, nil
}

type managedListener struct{ managed *process.Managed }

func (m *managedListener) Identity() state.ListenerIdentity { return m.managed.Identity() }
func (m *managedListener) Inspect(ctx context.Context) (state.LiveListenerProof, bool, error) {
	return m.managed.Inspect(ctx)
}
func (m *managedListener) ChildEnvironment(base []string) ([]string, error) {
	return m.managed.ChildEnvironment(base)
}
func (m *managedListener) Shutdown(ctx context.Context) error { return m.managed.Shutdown(ctx) }

type frozenPolicy struct {
	path   string
	policy proxycontract.Policy
	digest string
}

func (p frozenPolicy) Load(path string) (proxycontract.Policy, string, error) {
	if filepath.Clean(path) != p.path {
		return proxycontract.Policy{}, "", activation.ErrPolicyInvalid
	}
	return p.policy, p.digest, nil
}

type unavailablePolicy struct{}

func (unavailablePolicy) Load(string) (proxycontract.Policy, string, error) {
	return proxycontract.Policy{}, "", activation.ErrPolicyInvalid
}

type unavailableRunner struct{}

func (unavailableRunner) Run(context.Context, []string, []string) (int, error) {
	return 1, activation.ErrUnavailable
}

func (c *Commands) result(command, status, stateValue string, exit int) activation.Result {
	now := time.Now
	if c != nil && c.now != nil {
		now = c.now
	}
	return activation.Result{Command: command, ContractVersion: activation.ContractVersion, Status: status, State: stateValue, Timestamp: now().UTC().Format(time.RFC3339), ExitCode: exit}
}
func modeExit(err error) int {
	switch {
	case errors.Is(err, activation.ErrSessionUnsupported):
		return 2
	case errors.Is(err, activation.ErrCredentialUnavailable):
		return 3
	default:
		return 7
	}
}
func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

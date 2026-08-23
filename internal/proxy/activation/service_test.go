package activation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/proxy/router"
	"github.com/blazncloud/blazn/internal/proxy/state"
	"github.com/blazncloud/blazn/internal/proxycontract"
)

const testDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakePolicies struct {
	policy proxycontract.Policy
	digest string
	calls  int
}

func (f *fakePolicies) Load(string) (proxycontract.Policy, string, error) {
	f.calls++
	return f.policy, f.digest, nil
}

type fakeEnvironment struct {
	values       map[string]string
	markers      map[string]string
	configs      map[string]string
	publishCalls int
	publishErr   error
	modeErr      error
}

func newFakeEnvironment() *fakeEnvironment {
	return &fakeEnvironment{values: map[string]string{"OPENAI_API_KEY": "prior-openai", "HTTP_PROXY": "direct"}, markers: map[string]string{}, configs: map[string]string{"codex": "before", "claude": "before", "hermes": "before"}}
}
func (*fakeEnvironment) Platform() string { return "linux" }
func (*fakeEnvironment) SessionIdentity(context.Context) (string, error) {
	return "uid:1000/session:test", nil
}
func (f *fakeEnvironment) ResolveMode(value string) (string, string, error) {
	if f.modeErr != nil {
		return "", "", f.modeErr
	}
	if value != "auto" && value != "session" {
		return "", "", ErrUnavailable
	}
	return "session", "systemd_user_environment", nil
}
func (f *fakeEnvironment) Snapshot(_ context.Context, names []string) (map[string]PriorValue, error) {
	result := map[string]PriorValue{}
	for _, name := range names {
		value, ok := f.values[name]
		result[name] = PriorValue{ok, value}
	}
	return result, nil
}
func (f *fakeEnvironment) Publish(_ context.Context, _ string, values []PublishedValue) error {
	f.publishCalls++
	for _, item := range values {
		f.values[item.Name] = item.Value
		f.markers[item.Name] = item.Marker
	}
	return f.publishErr
}
func (f *fakeEnvironment) BaseEnvironment() []string {
	result := []string{"PATH=/bin", "HTTP_PROXY=direct"}
	for name, value := range f.values {
		result = append(result, name+"="+value)
	}
	return result
}
func (f *fakeEnvironment) CompareAndSet(_ context.Context, request state.CompareAndSetRequest) (state.CompareAndSetResult, error) {
	value, present := f.values[request.Name]
	if !present && request.PriorPresent {
		return state.CASAlreadyRestored, nil
	}
	if present && digest(value) == request.ExpectedValueDigest && f.markers[request.Name] == request.ActivationMarker {
		if request.PriorPresent {
			f.values[request.Name] = *request.PriorValue
		} else {
			delete(f.values, request.Name)
		}
		delete(f.markers, request.Name)
		return state.CASRestored, nil
	}
	if present && request.PriorPresent && value == *request.PriorValue {
		return state.CASAlreadyRestored, nil
	}
	if !present && !request.PriorPresent {
		return state.CASAlreadyRestored, nil
	}
	return state.CASConflict, nil
}

type fakeListener struct {
	identity state.ListenerIdentity
	token    string
	stopped  bool
}

func (f *fakeListener) Identity() state.ListenerIdentity { return f.identity }
func (f *fakeListener) ChildEnvironment(base []string) ([]string, error) {
	result := append([]string(nil), base...)
	for _, name := range state.EnvironmentNames {
		value := f.token
		if strings.HasSuffix(name, "BASE_URL") {
			value = "http://127.0.0.1:8123"
		}
		result = append(result, name+"="+value)
	}
	return result, nil
}
func (f *fakeListener) Shutdown(context.Context) error { f.stopped = true; return nil }

type fakeFactory struct {
	starts     int
	listeners  map[int]*fakeListener
	controller *fakeController
}

func (f *fakeFactory) Start(_ context.Context, _ proxycontract.Policy, _ string, metadata ListenerMetadata) (ManagedListener, error) {
	f.starts++
	listener := &fakeListener{token: "listener-secret-value", identity: state.ListenerIdentity{PID: 4000 + f.starts, ProcessStartIdentity: fmt.Sprintf("start-%d", f.starts), ExecutableIdentity: "exe-identity", Address: "127.0.0.1:8123", ListenerKeyFingerprint: testDigest}}
	f.listeners[listener.identity.PID] = listener
	f.controller.proofs[listener.identity.PID] = state.LiveListenerProof{PID: listener.identity.PID, ProcessStartIdentity: listener.identity.ProcessStartIdentity, ExecutableIdentity: listener.identity.ExecutableIdentity, BinaryDigest: testDigest, ListenerKeyFingerprint: testDigest, ActivationNonce: metadata.Nonce, OwnerUID: int(metadata.OwnerUID), Generation: metadata.Generation, Mode: metadata.Mode, SessionIdentity: metadata.SessionIdentity}
	return listener, nil
}

type fakeController struct {
	proofs     map[int]state.LiveListenerProof
	stops      int
	inspectErr error
}

func (f *fakeController) Inspect(_ context.Context, pid int) (state.LiveListenerProof, bool, error) {
	if f.inspectErr != nil {
		return state.LiveListenerProof{}, false, f.inspectErr
	}
	proof, ok := f.proofs[pid]
	return proof, ok, nil
}
func (f *fakeController) Stop(_ context.Context, proof state.LiveListenerProof) error {
	current, ok := f.proofs[proof.PID]
	if !ok || current != proof {
		return errors.New("identity mismatch")
	}
	delete(f.proofs, proof.PID)
	f.stops++
	return nil
}

type fakeStore struct {
	journal       *state.Journal
	stale         bool
	activateFault string
	panicRecover  bool
	reconcileErr  error
	recoverErr    error
}

func (f *fakeStore) Activate(_ context.Context, journal *state.Journal, _ string, publish func() error) error {
	copy := *journal
	f.journal = &copy
	if f.activateFault == "prepared" {
		return errors.New("prepared crash")
	}
	f.journal.State = "publishing"
	if err := publish(); err != nil {
		f.journal.State = "recovery_required"
		return err
	}
	if f.activateFault == "published" {
		return errors.New("published crash")
	}
	f.journal.State = "active"
	return nil
}
func (f *fakeStore) Reconcile(context.Context) (state.Reconciliation, error) {
	if f.reconcileErr != nil {
		return state.Reconciliation{}, f.reconcileErr
	}
	if f.stale {
		return state.Reconciliation{State: state.ReconciliationRecoveryRequired, LifecycleState: "recovery_required"}, state.ErrRecoveryRequired
	}
	if f.journal == nil {
		return state.Reconciliation{State: state.ReconciliationInactive}, nil
	}
	value := f.journal
	proof := state.LiveListenerProof{PID: value.Listener.PID, ProcessStartIdentity: value.Listener.ProcessStartIdentity, ExecutableIdentity: value.Listener.ExecutableIdentity, BinaryDigest: value.Binary.Digest, ListenerKeyFingerprint: value.Listener.ListenerKeyFingerprint, ActivationNonce: value.Nonce, OwnerUID: value.OwnerUID, Generation: value.Generation, Mode: value.Mode, SessionIdentity: value.SessionIdentity}
	result := state.Reconciliation{State: state.ReconciliationActive, ActivationID: value.ActivationID, Generation: value.Generation, LifecycleState: value.State, PolicyDigest: value.Policy.Digest, Mode: value.Mode, ListenerProof: &proof}
	if value.State != "active" {
		result.State = state.ReconciliationRecoveryRequired
		return result, state.ErrRecoveryRequired
	}
	return result, nil
}
func (f *fakeStore) Recover(ctx context.Context, environment state.EnvironmentRestorer, controller state.ListenerController) (state.RecoveryResult, error) {
	if f.panicRecover {
		panic("injected")
	}
	if f.recoverErr != nil {
		return state.RecoveryResult{}, f.recoverErr
	}
	if f.journal == nil {
		return state.RecoveryResult{Status: state.RecoveryNotActive}, nil
	}
	journal := f.journal
	expected := state.LiveListenerProof{PID: journal.Listener.PID, ProcessStartIdentity: journal.Listener.ProcessStartIdentity, ExecutableIdentity: journal.Listener.ExecutableIdentity, BinaryDigest: journal.Binary.Digest, ListenerKeyFingerprint: journal.Listener.ListenerKeyFingerprint, ActivationNonce: journal.Nonce, OwnerUID: journal.OwnerUID, Generation: journal.Generation, Mode: journal.Mode, SessionIdentity: journal.SessionIdentity}
	current, live, err := controller.Inspect(ctx, expected.PID)
	if err != nil {
		return state.RecoveryResult{Status: state.RecoveryRequired}, state.ErrRecoveryRequired
	}
	if live && current == expected {
		if err := controller.Stop(ctx, expected); err != nil {
			return state.RecoveryResult{Status: state.RecoveryRequired}, state.ErrRecoveryRequired
		}
	}
	result := state.RecoveryResult{Status: state.RecoveryComplete}
	for _, item := range journal.Environment {
		cas, _ := environment.CompareAndSet(ctx, state.CompareAndSetRequest{Name: item.Name, ExpectedValueDigest: item.DesiredValueDigest, ActivationMarker: item.ActivationMarker, PriorPresent: item.PriorPresent, PriorValue: item.PriorValue})
		if cas == state.CASConflict {
			result.Status = state.RecoveryRequired
			result.ConflictedEnvironment = append(result.ConflictedEnvironment, item.Name)
		}
	}
	if result.Status == state.RecoveryRequired {
		return result, state.ErrRecoveryRequired
	}
	f.journal = nil
	return result, nil
}

type fakeRunner struct {
	argv, environment []string
	exit              int
	err               error
}

func (f *fakeRunner) Run(_ context.Context, argv, environment []string) (int, error) {
	f.argv = append([]string(nil), argv...)
	f.environment = append([]string(nil), environment...)
	return f.exit, f.err
}

type fakeEvents struct{}

func (fakeEvents) Read(context.Context, string, bool) ([]Event, error) {
	return []Event{{EventID: "90000000-0000-4000-8000-000000000001", Cursor: "1", Timestamp: "2026-08-22T00:00:00Z", Type: proxycontract.EventRequestStarted, ActivationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", LogicalRequestID: "90000000-0000-4000-8000-000000000002", Attempt: 1, Protocol: proxycontract.ProtocolOpenAIChat, ModelAlias: "company-assistant", Policy: proxycontract.PolicyIdentity{ID: "11111111-1111-4111-8111-111111111111", Version: 1, Digest: testDigest}, RouteID: "33333333-3333-4333-8333-333333333333", DestinationClass: proxycontract.DestinationLocalNode, Outcome: proxycontract.OutcomeSuccess, ReasonCode: proxycontract.EventReasonNone, LatencyMS: 0}}, nil
}

type integrationCredentials struct{}

func (integrationCredentials) DestinationCredential(context.Context, string) (string, error) {
	return "destination-secret", nil
}

type integrationDNS struct{}

func (integrationDNS) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if host == "127.0.0.1" {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}

type integrationIdentity struct{}

func (integrationIdentity) Identity(_ context.Context, address, fingerprint string, _ ListenerMetadata) (state.ListenerIdentity, error) {
	return state.ListenerIdentity{PID: 777, ProcessStartIdentity: "start", ExecutableIdentity: "binary", Address: address, ListenerKeyFingerprint: fingerprint}, nil
}

func testService(t *testing.T) (*Service, *fakeStore, *fakeEnvironment, *fakeFactory, *fakePolicies, *fakeRunner) {
	t.Helper()
	policy := fixturePolicy(t)
	store := &fakeStore{}
	environment := newFakeEnvironment()
	controller := &fakeController{proofs: map[int]state.LiveListenerProof{}}
	factory := &fakeFactory{listeners: map[int]*fakeListener{}, controller: controller}
	policies := &fakePolicies{policy: policy, digest: testDigest}
	runner := &fakeRunner{}
	service, err := New(Dependencies{Store: store, Environment: environment, Listeners: factory, Controller: controller, Policies: policies, Runner: runner, Events: fakeEvents{}, Binary: state.BinaryIdentity{Path: "/usr/local/bin/blazn", Digest: testDigest}, OwnerUID: 1000, Now: func() time.Time { return time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	return service, store, environment, factory, policies, runner
}
func fixturePolicy(t *testing.T) proxycontract.Policy {
	t.Helper()
	file, err := os.Open(filepath.Join("..", "..", "..", "packages", "contracts", "proxy", "fixtures", "poc-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	policy, err := proxycontract.DecodePolicy(file)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestOnIsJournaledIdempotentAndOffRestoresDirectConnectivity(t *testing.T) {
	service, store, environment, factory, policies, _ := testService(t)
	configs := mapsClone(environment.configs)
	result, err := service.On(context.Background(), "policy.json", "auto")
	if err != nil || result.State != "active" || store.journal == nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if environment.values["HTTP_PROXY"] != "direct" || environment.configs["codex"] != configs["codex"] {
		t.Fatal("activation mutated direct connectivity or application configuration")
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "listener-secret-value") || strings.Contains(fmt.Sprintf("%+v", store.journal), "listener-secret-value") {
		t.Fatal("listener credential leaked outside environment")
	}
	again, err := service.On(context.Background(), "policy.json", "auto")
	if err != nil || again.Status != "idempotent" || factory.starts != 1 {
		t.Fatalf("idempotency=%+v err=%v starts=%d", again, err, factory.starts)
	}
	beforeCalls := policies.calls
	off, err := service.Off(context.Background())
	if err != nil || off.State != "inactive" || policies.calls != beforeCalls {
		t.Fatalf("off=%+v err=%v policyCalls=%d", off, err, policies.calls)
	}
	if environment.values["OPENAI_API_KEY"] != "prior-openai" || environment.values["HTTP_PROXY"] != "direct" || store.journal != nil {
		t.Fatal("off did not restore exact direct state")
	}
}

func TestDifferentPolicyStaleStateAndCrashRecoveryFailClosed(t *testing.T) {
	service, store, environment, _, policies, _ := testService(t)
	if _, err := service.On(context.Background(), "policy.json", "auto"); err != nil {
		t.Fatal(err)
	}
	policies.digest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if result, err := service.On(context.Background(), "other.json", "auto"); !errors.Is(err, ErrDifferentPolicy) || result.ExitCode != 6 {
		t.Fatalf("different policy result=%+v err=%v", result, err)
	}
	store.stale = true
	if result, err := service.Status(context.Background()); !errors.Is(err, state.ErrRecoveryRequired) || result.ExitCode != 9 {
		t.Fatalf("stale result=%+v err=%v", result, err)
	}
	store.stale = false
	if _, err := service.Off(context.Background()); err != nil {
		t.Fatal(err)
	}
	service, store, environment, _, _, _ = testService(t)
	store.activateFault = "published"
	if result, err := service.On(context.Background(), "policy.json", "auto"); err == nil || result.State != "recovery_required" {
		t.Fatalf("crash result=%+v err=%v", result, err)
	}
	if _, err := service.Off(context.Background()); err != nil {
		t.Fatal(err)
	}
	if environment.values["OPENAI_API_KEY"] != "prior-openai" {
		t.Fatal("crash recovery did not restore prior value")
	}
}

func TestActivationCrashBoundariesNeverLoseRecoveryAuthority(t *testing.T) {
	for _, testCase := range []struct {
		name, fault     string
		publishError    bool
		expectPublished bool
	}{{"after prepared journal", "prepared", false, false}, {"publication failure", "", true, true}, {"after publication", "published", false, true}} {
		t.Run(testCase.name, func(t *testing.T) {
			service, store, environment, factory, _, _ := testService(t)
			store.activateFault = testCase.fault
			if testCase.publishError {
				environment.publishErr = errors.New("injected publication failure")
			}
			before := mapsClone(environment.configs)
			result, err := service.On(context.Background(), "policy.json", "auto")
			if err == nil || result.State != "recovery_required" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if (environment.publishCalls > 0) != testCase.expectPublished {
				t.Fatalf("publish calls=%d", environment.publishCalls)
			}
			if environment.configs["codex"] != before["codex"] || environment.values["HTTP_PROXY"] != "direct" {
				t.Fatal("crash path changed application config or direct connectivity")
			}
			for _, listener := range factory.listeners {
				if !listener.stopped {
					t.Fatal("failed activation left embedded listener running")
				}
			}
			environment.publishErr = nil
			if _, offErr := service.Off(context.Background()); offErr != nil {
				t.Fatal(offErr)
			}
			if environment.values["OPENAI_API_KEY"] != "prior-openai" {
				t.Fatal("recovery did not restore exact prior environment")
			}
		})
	}
}

func TestRunUsesExactArgvAndAlwaysRestores(t *testing.T) {
	service, store, environment, _, _, runner := testService(t)
	runner.exit = 23
	result, err := service.Run(context.Background(), "policy.json", []string{"tool", "a b", "$HOME", ";rm"})
	if err != nil || result.ExitCode != 23 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if strings.Join(runner.argv, "|") != "tool|a b|$HOME|;rm" || store.journal != nil {
		t.Fatalf("argv=%q journal=%v", runner.argv, store.journal)
	}
	joined := strings.Join(runner.environment, "\n")
	if !strings.Contains(joined, "OPENAI_API_KEY=listener-secret-value") || environment.values["OPENAI_API_KEY"] != "prior-openai" {
		t.Fatal("scoped environment was not exact or restored")
	}
}

func TestOffAndResetArePanicSafeAndNeedNoPolicyOrAPI(t *testing.T) {
	service, store, _, _, policies, _ := testService(t)
	store.panicRecover = true
	before := policies.calls
	if result, err := service.Off(context.Background()); !errors.Is(err, ErrRecovery) || result.ExitCode != 9 || policies.calls != before {
		t.Fatalf("panic off=%+v err=%v", result, err)
	}
	if result, err := service.Reset(context.Background(), false, false); err == nil || result.ExitCode != 2 {
		t.Fatalf("reset confirmation=%+v err=%v", result, err)
	}
	if result, err := service.Reset(context.Background(), true, false); !errors.Is(err, ErrRecovery) || result.ExitCode != 9 {
		t.Fatalf("panic reset=%+v err=%v", result, err)
	}
}

func TestCorruptOrAmbiguousStateReturnsRecoveryRequiredWithoutMutation(t *testing.T) {
	service, store, environment, _, policies, _ := testService(t)
	beforeValues, beforeConfigs, beforeCalls := mapsClone(environment.values), mapsClone(environment.configs), policies.calls
	store.reconcileErr = state.ErrOwnershipAmbiguous
	if result, err := service.Status(context.Background()); !errors.Is(err, ErrRecovery) || result.ExitCode != 9 || result.State != "recovery_required" {
		t.Fatalf("status=%+v err=%v", result, err)
	}
	store.reconcileErr, store.recoverErr = nil, state.ErrOwnershipAmbiguous
	if result, err := service.Off(context.Background()); !errors.Is(err, ErrRecovery) || result.ExitCode != 9 || result.State != "recovery_required" {
		t.Fatalf("off=%+v err=%v", result, err)
	}
	if policies.calls != beforeCalls || fmt.Sprint(environment.values) != fmt.Sprint(beforeValues) || fmt.Sprint(environment.configs) != fmt.Sprint(beforeConfigs) {
		t.Fatal("corrupt-state handling mutated user state or contacted policy/API")
	}
}

func TestAbruptListenerLossDoctorRoutesAndTail(t *testing.T) {
	service, store, environment, factory, _, _ := testService(t)
	if _, err := service.On(context.Background(), "policy.json", "auto"); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	delete(service.listeners, store.journal.Listener.PID)
	service.mu.Unlock()
	delete(factory.controller.proofs, store.journal.Listener.PID)
	if _, err := service.Off(context.Background()); err != nil {
		t.Fatal(err)
	}
	if environment.values["OPENAI_API_KEY"] != "prior-openai" {
		t.Fatal("abrupt listener loss did not restore environment")
	}
	doctor, err := service.Doctor(context.Background(), "policy.json")
	if err != nil || doctor.Status != "ready" {
		t.Fatalf("doctor=%+v err=%v", doctor, err)
	}
	routes, err := service.Routes(context.Background(), "policy.json")
	if err != nil || len(routes) != 2 || strings.Contains(fmt.Sprintf("%+v", routes), "node-route://") || strings.Contains(fmt.Sprintf("%+v", routes), "workspace-vault://") {
		t.Fatalf("routes=%+v err=%v", routes, err)
	}
	events, err := service.Tail(context.Background(), "", false)
	if err != nil || len(events) != 1 || strings.Contains(fmt.Sprintf("%+v", events), "secret") {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

func TestEmbeddedFactoryWiresAuthenticatedLoopbackRuntime(t *testing.T) {
	policy := fixturePolicy(t)
	factory := EmbeddedListenerFactory{Address: "127.0.0.1", Router: router.Config{Credentials: integrationCredentials{}, Resolver: router.EndpointResolver{DNS: integrationDNS{}}}, Identity: integrationIdentity{}}
	managed, err := factory.Start(context.Background(), policy, testDigest, ListenerMetadata{ActivationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Nonce: strings.Repeat("n", 32), Mode: "scoped_run", SessionIdentity: "session", Generation: 1, OwnerUID: 1000})
	if err != nil {
		t.Fatal(err)
	}
	environment, err := managed.ChildEnvironment([]string{"PATH=/bin"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(environment, "\n")
	if !strings.Contains(joined, "OPENAI_BASE_URL=http://127.0.0.1:") || strings.Contains(fmt.Sprintf("%+v", managed), "destination-secret") {
		t.Fatalf("environment or formatting is unsafe: %s", joined)
	}
	if !strings.HasPrefix(managed.Identity().ListenerKeyFingerprint, "sha256:") {
		t.Fatal("listener fingerprint missing")
	}
	if err := managed.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func mapsClone(source map[string]string) map[string]string {
	result := map[string]string{}
	for key, value := range source {
		result[key] = value
	}
	return result
}

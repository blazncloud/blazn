//go:build linux

package systemdenv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/proxy/activation"
	"github.com/blazncloud/blazn/internal/proxy/state"
)

const testActivationID = "123e4567-e89b-42d3-a456-426614174000"

type transportCall struct {
	operation string
	request   busRequest
	values    []string
}

type fakeTransport struct {
	mu sync.Mutex

	proof managerProof
	env   map[string]string
	calls []transportCall

	probeErr error
	getErr   error
	setErrAt int
	unsetErr error
	wait     string
	afterGet func(*fakeTransport, int)
	getCount int
}

func (f *fakeTransport) Probe(ctx context.Context, request busRequest) (managerProof, error) {
	f.record("probe", request, nil)
	if err := f.waitFor(ctx, "probe"); err != nil {
		return managerProof{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.proof, f.probeErr
}

func (f *fakeTransport) GetEnvironment(ctx context.Context, request busRequest) (managerProof, []string, error) {
	f.record("get", request, nil)
	if err := f.waitFor(ctx, "get"); err != nil {
		return managerProof{}, nil, err
	}
	f.mu.Lock()
	f.getCount++
	count := f.getCount
	hook := f.afterGet
	f.mu.Unlock()
	if hook != nil {
		hook(f, count)
	}
	f.mu.Lock()
	proof, getErr := f.proof, f.getErr
	values := make([]string, 0, len(f.env)+1)
	values = append(values, "UNRELATED=value")
	for _, name := range state.EnvironmentNames {
		if value, ok := f.env[name]; ok {
			values = append(values, name+"="+value)
		}
	}
	f.mu.Unlock()
	return proof, values, getErr
}

func (f *fakeTransport) SetEnvironment(ctx context.Context, request busRequest, values []string) (managerProof, error) {
	f.record("set", request, values)
	if err := f.waitFor(ctx, "set"); err != nil {
		return managerProof{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	setCount := 0
	for _, call := range f.calls {
		if call.operation == "set" {
			setCount++
		}
	}
	if f.setErrAt > 0 && setCount == f.setErrAt {
		return managerProof{}, errors.New("set transport failed with hidden material")
	}
	for _, entry := range values {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			return managerProof{}, errors.New("invalid fake assignment")
		}
		f.env[name] = value
	}
	return f.proof, nil
}

func (f *fakeTransport) UnsetEnvironment(ctx context.Context, request busRequest, names []string) (managerProof, error) {
	f.record("unset", request, names)
	if err := f.waitFor(ctx, "unset"); err != nil {
		return managerProof{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unsetErr != nil {
		return managerProof{}, f.unsetErr
	}
	for _, name := range names {
		delete(f.env, name)
	}
	return f.proof, nil
}

func (f *fakeTransport) record(operation string, request busRequest, values []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, transportCall{operation: operation, request: request, values: append([]string(nil), values...)})
}

func (f *fakeTransport) waitFor(ctx context.Context, operation string) error {
	f.mu.Lock()
	wait := f.wait == operation
	f.mu.Unlock()
	if wait {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func proof(uid int) managerProof {
	return managerProof{OwnerUID: uid, BusID: strings.Repeat("a", 32), ManagerOwner: ":1.42", DesktopInheritance: true}
}

func newTestAdapter(uid int, transport *fakeTransport) *Adapter {
	return &Adapter{uid: uid, timeout: time.Second, transport: transport}
}

func publishedValues() []activation.PublishedValue {
	values := make([]activation.PublishedValue, 0, len(state.EnvironmentNames))
	for index, name := range state.EnvironmentNames {
		values = append(values, activation.PublishedValue{Name: name, Value: fmt.Sprintf("private-value-%d", index), Marker: testActivationID + ":" + name})
	}
	return values
}

func TestAdapterUsesOSUIDBusAndRequiresDesktopCapability(t *testing.T) {
	const uid = 1801
	transport := &fakeTransport{proof: proof(uid), env: map[string]string{state.EnvironmentNames[0]: "", state.EnvironmentNames[2]: "prior"}}
	adapter := newTestAdapter(uid, transport)
	for _, name := range []string{"HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		t.Setenv(name, "/attacker/controlled")
	}

	mode, mechanism, err := adapter.ResolveMode("session")
	if err != nil || mode != "session" || mechanism != publicationMechanism {
		t.Fatalf("resolve mode=(%q,%q) error=%v", mode, mechanism, err)
	}
	identity, err := adapter.SessionIdentity(context.Background())
	if err != nil || identity != "uid:1801/systemd-user:"+strings.Repeat("a", 32)+"/:1.42" {
		t.Fatalf("identity=%q error=%v", identity, err)
	}
	snapshot, err := adapter.Snapshot(context.Background(), state.EnvironmentNames[:])
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot[state.EnvironmentNames[0]].Present || snapshot[state.EnvironmentNames[0]].Value != "" {
		t.Fatalf("empty prior was not preserved: %#v", snapshot[state.EnvironmentNames[0]])
	}
	if snapshot[state.EnvironmentNames[1]].Present {
		t.Fatalf("absent prior became present: %#v", snapshot[state.EnvironmentNames[1]])
	}
	if !snapshot[state.EnvironmentNames[2]].Present || snapshot[state.EnvironmentNames[2]].Value != "prior" {
		t.Fatalf("prior value changed: %#v", snapshot[state.EnvironmentNames[2]])
	}

	transport.mu.Lock()
	defer transport.mu.Unlock()
	for _, call := range transport.calls {
		if call.request != (busRequest{Path: "/run/user/1801/bus", UID: uid}) {
			t.Fatalf("request=%#v", call.request)
		}
	}

	managerOnly := &fakeTransport{proof: proof(uid), env: map[string]string{}}
	managerOnly.proof.DesktopInheritance = false
	unsupported := newTestAdapter(uid, managerOnly)
	if _, _, err := unsupported.ResolveMode("session"); !errors.Is(err, activation.ErrSessionUnsupported) {
		t.Fatalf("manager-only capability returned %v", err)
	}
	if _, _, err := unsupported.ResolveMode("auto"); !errors.Is(err, activation.ErrSessionUnsupported) {
		t.Fatalf("auto manager-only capability returned %v", err)
	}
}

func TestSnapshotRejectsAnythingExceptFrozenFiveInOrder(t *testing.T) {
	const uid = 1802
	transport := &fakeTransport{proof: proof(uid), env: map[string]string{}}
	adapter := newTestAdapter(uid, transport)
	bad := [][]string{
		nil,
		append([]string(nil), state.EnvironmentNames[:4]...),
		append(append([]string(nil), state.EnvironmentNames[:]...), "HTTP_PROXY"),
		{state.EnvironmentNames[1], state.EnvironmentNames[0], state.EnvironmentNames[2], state.EnvironmentNames[3], state.EnvironmentNames[4]},
	}
	for _, names := range bad {
		if _, err := adapter.Snapshot(context.Background(), names); !errors.Is(err, activation.ErrUnavailable) {
			t.Fatalf("names=%v error=%v", names, err)
		}
	}
	if len(transport.calls) != 0 {
		t.Fatalf("invalid snapshots reached transport: %#v", transport.calls)
	}
}

func TestPublishSetsExactlyOneFrozenVariablePerDirectCall(t *testing.T) {
	const uid = 1803
	transport := &fakeTransport{proof: proof(uid), env: map[string]string{}}
	adapter := newTestAdapter(uid, transport)
	if _, _, err := adapter.ResolveMode("session"); err != nil {
		t.Fatal(err)
	}
	values := publishedValues()
	if err := adapter.Publish(context.Background(), publicationMechanism, values); err != nil {
		t.Fatal(err)
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.calls) != 1+len(state.EnvironmentNames) || transport.calls[0].operation != "probe" {
		t.Fatalf("calls=%#v", transport.calls)
	}
	for index, name := range state.EnvironmentNames {
		call := transport.calls[index+1]
		want := []string{name + "=" + values[index].Value}
		if call.operation != "set" || !reflect.DeepEqual(call.values, want) {
			t.Fatalf("call[%d]=%#v want %v", index, call, want)
		}
		if transport.env[name] != values[index].Value {
			t.Fatalf("published %s=%q", name, transport.env[name])
		}
	}
	for _, forbidden := range []string{"HTTP_PROXY", "HTTPS_PROXY", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS"} {
		if _, exists := transport.env[forbidden]; exists {
			t.Fatalf("published forbidden variable %s", forbidden)
		}
	}
}

func TestPublishPartialFailureAtEveryVariableLeavesJournalRecoverablePrefix(t *testing.T) {
	const uid = 1804
	values := publishedValues()
	for failure := 1; failure <= len(values); failure++ {
		t.Run(fmt.Sprintf("variable-%d", failure), func(t *testing.T) {
			transport := &fakeTransport{proof: proof(uid), env: map[string]string{}, setErrAt: failure}
			adapter := newTestAdapter(uid, transport)
			if _, _, err := adapter.ResolveMode("session"); err != nil {
				t.Fatal(err)
			}
			err := adapter.Publish(context.Background(), publicationMechanism, values)
			if !errors.Is(err, activation.ErrUnavailable) || strings.Contains(err.Error(), "private-value") {
				t.Fatalf("partial failure error=%v", err)
			}
			transport.mu.Lock()
			defer transport.mu.Unlock()
			setCalls := 0
			for _, call := range transport.calls {
				if call.operation == "set" {
					setCalls++
				}
			}
			if setCalls != failure {
				t.Fatalf("set calls=%d want %d", setCalls, failure)
			}
			for index, name := range state.EnvironmentNames {
				got, present := transport.env[name]
				wantPresent := index < failure-1
				if present != wantPresent || (present && got != values[index].Value) {
					t.Fatalf("%s present=%t value=%q want prefix before %d", name, present, got, failure)
				}
			}
		})
	}
}

func TestPublishRejectsWrongMechanismShapeMarkerAndUnsafeValueWithoutCalls(t *testing.T) {
	const uid = 1805
	tests := map[string]func([]activation.PublishedValue) (string, []activation.PublishedValue){
		"mechanism": func(values []activation.PublishedValue) (string, []activation.PublishedValue) {
			return "process_environment", values
		},
		"missing": func(values []activation.PublishedValue) (string, []activation.PublishedValue) {
			return publicationMechanism, values[:4]
		},
		"swapped": func(values []activation.PublishedValue) (string, []activation.PublishedValue) {
			values[0], values[1] = values[1], values[0]
			return publicationMechanism, values
		},
		"marker": func(values []activation.PublishedValue) (string, []activation.PublishedValue) {
			values[0].Marker = testActivationID + ":HTTP_PROXY"
			return publicationMechanism, values
		},
		"newline": func(values []activation.PublishedValue) (string, []activation.PublishedValue) {
			values[0].Value = "hidden\nHTTP_PROXY=attacker"
			return publicationMechanism, values
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			transport := &fakeTransport{proof: proof(uid), env: map[string]string{}}
			mechanism, values := mutate(publishedValues())
			if err := newTestAdapter(uid, transport).Publish(context.Background(), mechanism, values); !errors.Is(err, activation.ErrSessionUnsupported) {
				t.Fatalf("error=%v", err)
			}
			if len(transport.calls) != 0 {
				t.Fatalf("rejected publish made calls: %#v", transport.calls)
			}
		})
	}
}

func TestPublishRequiresPreviouslyProvedDesktopCapability(t *testing.T) {
	const uid = 1812
	transport := &fakeTransport{proof: proof(uid), env: map[string]string{}}
	adapter := newTestAdapter(uid, transport)
	if err := adapter.Publish(context.Background(), publicationMechanism, publishedValues()); !errors.Is(err, activation.ErrSessionUnsupported) {
		t.Fatalf("unproved publish error=%v", err)
	}
	if len(transport.calls) != 0 || len(transport.env) != 0 {
		t.Fatalf("unproved publish reached transport: calls=%#v env=%#v", transport.calls, transport.env)
	}
}

func TestCompareAndSetRestoresExactAbsentEmptyAndValue(t *testing.T) {
	const uid = 1806
	desired := "blazn-desired-private-value"
	for _, testCase := range []struct {
		name         string
		priorPresent bool
		prior        string
		operation    string
		argument     string
	}{
		{name: "absent", operation: "unset", argument: state.EnvironmentNames[0]},
		{name: "empty", priorPresent: true, prior: "", operation: "set", argument: state.EnvironmentNames[0] + "="},
		{name: "value", priorPresent: true, prior: "original", operation: "set", argument: state.EnvironmentNames[0] + "=original"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			transport := &fakeTransport{proof: proof(uid), env: map[string]string{state.EnvironmentNames[0]: desired}}
			adapter := newTestAdapter(uid, transport)
			if _, _, err := adapter.ResolveMode("session"); err != nil {
				t.Fatal(err)
			}
			request := casRequest(state.EnvironmentNames[0], desired, testCase.priorPresent, testCase.prior)
			result, err := adapter.CompareAndSet(context.Background(), request)
			if err != nil || result != state.CASRestored {
				t.Fatalf("result=%s error=%v", result, err)
			}
			transport.mu.Lock()
			defer transport.mu.Unlock()
			operations := make([]transportCall, 0)
			for _, call := range transport.calls {
				if call.operation != "probe" && call.operation != "get" {
					operations = append(operations, call)
				}
			}
			if len(operations) != 1 || operations[0].operation != testCase.operation || !reflect.DeepEqual(operations[0].values, []string{testCase.argument}) {
				t.Fatalf("mutation calls=%#v", operations)
			}
			value, present := transport.env[state.EnvironmentNames[0]]
			if present != testCase.priorPresent || (present && value != testCase.prior) {
				t.Fatalf("restored present=%t value=%q", present, value)
			}
		})
	}
}

func TestCompareAndSetLetsExternalChangesWin(t *testing.T) {
	const uid = 1807
	name := state.EnvironmentNames[1]
	tests := []struct {
		name    string
		current map[string]string
		prior   *string
		want    state.CompareAndSetResult
	}{
		{name: "different live value", current: map[string]string{name: "user-change"}, want: state.CASConflict},
		{name: "already absent", current: map[string]string{}, want: state.CASAlreadyRestored},
		{name: "already empty", current: map[string]string{name: ""}, prior: stringPointer(""), want: state.CASAlreadyRestored},
		{name: "already prior value", current: map[string]string{name: "prior"}, prior: stringPointer("prior"), want: state.CASAlreadyRestored},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			transport := &fakeTransport{proof: proof(uid), env: testCase.current}
			adapter := newTestAdapter(uid, transport)
			if _, _, err := adapter.ResolveMode("session"); err != nil {
				t.Fatal(err)
			}
			request := casRequest(name, "blazn-desired", testCase.prior != nil, "")
			request.PriorValue = testCase.prior
			result, err := adapter.CompareAndSet(context.Background(), request)
			if err != nil || result != testCase.want {
				t.Fatalf("result=%s error=%v want=%s", result, err, testCase.want)
			}
			for _, call := range transport.calls {
				if call.operation == "set" || call.operation == "unset" {
					t.Fatalf("external/prior value was mutated: %#v", call)
				}
			}
		})
	}

	transport := &fakeTransport{proof: proof(uid), env: map[string]string{name: "blazn-desired"}}
	transport.afterGet = func(current *fakeTransport, count int) {
		if count == 2 { // post-restore verification simulates a concurrent user change
			current.mu.Lock()
			current.env[name] = "user-after-restore"
			current.mu.Unlock()
		}
	}
	adapter := newTestAdapter(uid, transport)
	if _, _, err := adapter.ResolveMode("session"); err != nil {
		t.Fatal(err)
	}
	result, err := adapter.CompareAndSet(context.Background(), casRequest(name, "blazn-desired", false, ""))
	if err != nil || result != state.CASConflict || transport.env[name] != "user-after-restore" {
		t.Fatalf("post-restore external change result=%s error=%v env=%q", result, err, transport.env[name])
	}
}

func TestCompareAndSetRequiresValidatedJournalDigestAndExactMarker(t *testing.T) {
	const uid = 1808
	name := state.EnvironmentNames[0]
	tests := map[string]func(*state.CompareAndSetRequest){
		"foreign variable": func(request *state.CompareAndSetRequest) { request.Name = "HTTP_PROXY" },
		"bad digest":       func(request *state.CompareAndSetRequest) { request.ExpectedValueDigest = "sha256:secret" },
		"bad activation":   func(request *state.CompareAndSetRequest) { request.ActivationMarker = "not-a-journal-marker:" + name },
		"wrong marker name": func(request *state.CompareAndSetRequest) {
			request.ActivationMarker = testActivationID + ":" + state.EnvironmentNames[1]
		},
		"presence mismatch": func(request *state.CompareAndSetRequest) { request.PriorPresent = true },
	}
	for testName, mutate := range tests {
		t.Run(testName, func(t *testing.T) {
			transport := &fakeTransport{proof: proof(uid), env: map[string]string{name: "desired"}}
			request := casRequest(name, "desired", false, "")
			mutate(&request)
			result, err := newTestAdapter(uid, transport).CompareAndSet(context.Background(), request)
			if result != state.CASConflict || !errors.Is(err, activation.ErrUnavailable) || len(transport.calls) != 0 {
				t.Fatalf("result=%s error=%v calls=%#v", result, err, transport.calls)
			}
		})
	}
}

func TestAdapterRejectsOutageWrongUIDOwnerBusSessionAndCapabilitySwitch(t *testing.T) {
	const uid = 1809
	tests := map[string]func(*fakeTransport){
		"outage":            func(transport *fakeTransport) { transport.getErr = errors.New("private outage detail") },
		"wrong uid":         func(transport *fakeTransport) { transport.proof.OwnerUID++ },
		"manager restart":   func(transport *fakeTransport) { transport.proof.ManagerOwner = ":1.43" },
		"bus restart":       func(transport *fakeTransport) { transport.proof.BusID = strings.Repeat("b", 32) },
		"invalid owner":     func(transport *fakeTransport) { transport.proof.ManagerOwner = managerName },
		"capability switch": func(transport *fakeTransport) { transport.proof.DesktopInheritance = false },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			transport := &fakeTransport{proof: proof(uid), env: map[string]string{}}
			adapter := newTestAdapter(uid, transport)
			if _, _, err := adapter.ResolveMode("session"); err != nil {
				t.Fatal(err)
			}
			mutate(transport)
			_, err := adapter.Snapshot(context.Background(), state.EnvironmentNames[:])
			if !errors.Is(err, activation.ErrUnavailable) || strings.Contains(fmt.Sprint(err), "private outage detail") {
				t.Fatalf("snapshot error=%v", err)
			}
		})
	}
}

func TestAdapterTimeoutAndCancellationAreBoundedAndTyped(t *testing.T) {
	const uid = 1810
	t.Run("resolve timeout", func(t *testing.T) {
		transport := &fakeTransport{proof: proof(uid), env: map[string]string{}, wait: "probe"}
		adapter := newTestAdapter(uid, transport)
		adapter.timeout = 10 * time.Millisecond
		started := time.Now()
		if _, _, err := adapter.ResolveMode("session"); !errors.Is(err, activation.ErrSessionUnsupported) {
			t.Fatalf("error=%v", err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("timeout took %s", elapsed)
		}
	})

	t.Run("caller cancellation", func(t *testing.T) {
		transport := &fakeTransport{proof: proof(uid), env: map[string]string{}, wait: "get"}
		adapter := newTestAdapter(uid, transport)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := adapter.Snapshot(ctx, state.EnvironmentNames[:])
		if !errors.Is(err, activation.ErrUnavailable) || !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestAdapterErrorsAndFormattingNeverExposeValuesOrAlternateAuthority(t *testing.T) {
	const uid = 1811
	secret := "formatting-private-value"
	transport := &fakeTransport{proof: proof(uid), env: map[string]string{}, setErrAt: 1}
	adapter := newTestAdapter(uid, transport)
	if _, _, err := adapter.ResolveMode("session"); err != nil {
		t.Fatal(err)
	}
	values := publishedValues()
	values[0].Value = secret
	err := adapter.Publish(context.Background(), publicationMechanism, values)
	encoded, marshalErr := json.Marshal(adapter)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	formatted := fmt.Sprintf("%v %#v %s", adapter, adapter, encoded)
	formatted += fmt.Sprintf(" %v %#v", err, err)
	for _, forbidden := range []string{secret, "hidden material", "systemctl", "launchctl", "HTTP_PROXY", "HTTPS_PROXY"} {
		if strings.Contains(formatted, forbidden) {
			t.Fatalf("formatted output exposed forbidden material %q: %s", forbidden, formatted)
		}
	}

	typeOfAdapter := reflect.TypeOf(Adapter{})
	for index := 0; index < typeOfAdapter.NumField(); index++ {
		field := strings.ToLower(typeOfAdapter.Field(index).Name)
		for _, forbidden := range []string{"fallback", "command", "argv", "provider", "proxy", "certificate", "trust", "config"} {
			if strings.Contains(field, forbidden) {
				t.Fatalf("adapter gained alternate authority field %q", field)
			}
		}
	}
	for _, forbidden := range []string{"Switch", "Fallback", "Configure", "Trust", "Certificate", "Proxy"} {
		if _, exists := reflect.TypeOf(adapter).MethodByName(forbidden); exists {
			t.Fatalf("adapter exposes forbidden method %s", forbidden)
		}
	}
	if adapter.BaseEnvironment() != nil {
		t.Fatalf("adapter exposed application environment: %#v", adapter.BaseEnvironment())
	}
}

func TestProductionAdapterIsUnwiredOSUIDDirectDBusAndEnvironmentIndependent(t *testing.T) {
	for _, name := range []string{"HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		t.Setenv(name, "/attacker/controlled")
	}
	adapter := New()
	if adapter.uid != os.Getuid() || adapter.timeout != defaultTimeout {
		t.Fatalf("uid=%d timeout=%s", adapter.uid, adapter.timeout)
	}
	if _, ok := adapter.transport.(dbusTransport); !ok {
		t.Fatalf("transport=%T", adapter.transport)
	}
	if adapter.request() != (busRequest{Path: "/run/user/" + fmt.Sprint(os.Getuid()) + "/bus", UID: os.Getuid()}) {
		t.Fatalf("request=%#v", adapter.request())
	}
	productionProof := managerProof{OwnerUID: os.Getuid(), BusID: strings.Repeat("a", 32), ManagerOwner: ":1.2"}
	if productionProof.DesktopInheritance {
		t.Fatal("production discovery unexpectedly claims desktop inheritance")
	}
}

func TestEnvironmentParserRejectsAmbiguousAndUnsafeWireValues(t *testing.T) {
	name := state.EnvironmentNames[0]
	for _, entries := range [][]string{
		{name},
		{"=value"},
		{name + "=one", name + "=two"},
		{name + "=value\nHTTP_PROXY=attacker"},
		{"UNRELATED"},
	} {
		if _, err := parseEnvironment(entries); err == nil {
			t.Fatalf("entries unexpectedly accepted: %q", entries)
		}
	}
	values, err := parseEnvironment([]string{name + "=", "UNRELATED=value=with=equals"})
	value, present := values[name]
	if err != nil || !present || value != "" {
		t.Fatalf("empty exact value parse=%#v error=%v", values, err)
	}
}

func casRequest(name, desired string, priorPresent bool, prior string) state.CompareAndSetRequest {
	request := state.CompareAndSetRequest{
		Name:                name,
		ExpectedValueDigest: valueDigest(desired),
		ActivationMarker:    testActivationID + ":" + name,
		PriorPresent:        priorPresent,
	}
	if priorPresent {
		request.PriorValue = stringPointer(prior)
	}
	return request
}

func stringPointer(value string) *string { return &value }

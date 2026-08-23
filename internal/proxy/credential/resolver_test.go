package credential_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/blazncloud/blazn/internal/proxy/activation"
	"github.com/blazncloud/blazn/internal/proxy/credential"
	"github.com/blazncloud/blazn/internal/proxy/state"
	"github.com/blazncloud/blazn/internal/proxycontract"
)

type recordingBackend struct {
	mu      sync.Mutex
	values  map[string][]byte
	calls   map[string]int
	started chan string
	release <-chan struct{}
	err     error
}

func (b *recordingBackend) Lookup(ctx context.Context, ref string) ([]byte, error) {
	b.mu.Lock()
	b.calls[ref]++
	b.mu.Unlock()
	if b.started != nil {
		b.started <- ref
	}
	if b.release != nil {
		select {
		case <-b.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if b.err != nil {
		return nil, b.err
	}
	return b.values[ref], nil
}

func TestResolveDispatchesConcurrentlyDeduplicatesAndSnapshotsCopies(t *testing.T) {
	release := make(chan struct{})
	started := make(chan string, 3)
	nodeValues := map[string][]byte{
		"node-route://alpha": []byte("node-alpha-secret"),
		"node-route://beta":  []byte("node-beta-secret"),
	}
	vaultValues := map[string][]byte{
		"workspace-vault://team/provider": []byte("vault-secret"),
	}
	node := &recordingBackend{values: nodeValues, calls: map[string]int{}, started: started, release: release}
	vault := &recordingBackend{values: vaultValues, calls: map[string]int{}, started: started, release: release}
	policy := policyWithRoutes(
		route(proxycontract.DestinationLocalNode, "node-route://alpha"),
		route(proxycontract.DestinationLocalNode, "node-route://alpha"),
		route(proxycontract.DestinationLocalNode, "node-route://beta"),
		route(proxycontract.DestinationProvider, "workspace-vault://team/provider"),
	)

	type outcome struct {
		snapshot *credential.Snapshot
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		snapshot, err := (credential.Resolver{NodeRoute: node, WorkspaceVault: vault}).Resolve(context.Background(), policy)
		done <- outcome{snapshot: snapshot, err: err}
	}()
	seen := map[string]bool{}
	for range 3 {
		seen[<-started] = true
	}
	if len(seen) != 3 {
		t.Fatalf("unique lookups did not all start concurrently: %v", seen)
	}
	close(release)
	resolved := <-done
	if resolved.err != nil {
		t.Fatal(resolved.err)
	}
	for ref, want := range map[string]string{
		"node-route://alpha":              "node-alpha-secret",
		"node-route://beta":               "node-beta-secret",
		"workspace-vault://team/provider": "vault-secret",
	} {
		got, err := resolved.snapshot.DestinationCredential(context.Background(), ref)
		if err != nil || got != want {
			t.Fatalf("credential lookup for %s = %q, %v", ref, got, err)
		}
	}
	node.mu.Lock()
	if node.calls["node-route://alpha"] != 1 || node.calls["node-route://beta"] != 1 {
		t.Fatalf("node calls were not exact-once: %v", node.calls)
	}
	node.mu.Unlock()
	vault.mu.Lock()
	if vault.calls["workspace-vault://team/provider"] != 1 {
		t.Fatalf("vault calls were not exact-once: %v", vault.calls)
	}
	vault.mu.Unlock()
	for _, values := range []map[string][]byte{nodeValues, vaultValues} {
		for _, transient := range values {
			for _, current := range transient {
				if current != 0 {
					t.Fatal("transient backend buffer was not zeroed")
				}
			}
		}
	}
}

func TestSnapshotSupportsConcurrentCachedReadsWithoutBackendLookups(t *testing.T) {
	ref := "node-route://alpha"
	backend := &recordingBackend{values: map[string][]byte{ref: []byte("concurrent-secret")}, calls: map[string]int{}}
	snapshot, err := (credential.Resolver{NodeRoute: backend}).Resolve(context.Background(), policyWithRoutes(route(proxycontract.DestinationLocalNode, ref)))
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for range 64 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			value, lookupErr := snapshot.DestinationCredential(context.Background(), ref)
			if lookupErr != nil || value != "concurrent-secret" {
				t.Errorf("cached read = %q, %v", value, lookupErr)
			}
		}()
	}
	wait.Wait()
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.calls[ref] != 1 {
		t.Fatalf("backend called %d times", backend.calls[ref])
	}
}

func TestResolveRejectsInvalidMixedAndUnconfiguredReferences(t *testing.T) {
	available := &recordingBackend{values: map[string][]byte{}, calls: map[string]int{}}
	tests := []struct {
		name     string
		resolver credential.Resolver
		policy   proxycontract.Policy
	}{
		{"empty policy", credential.Resolver{NodeRoute: available, WorkspaceVault: available}, policyWithRoutes()},
		{"mixed local vault reference", credential.Resolver{NodeRoute: available, WorkspaceVault: available}, policyWithRoutes(route(proxycontract.DestinationLocalNode, "workspace-vault://team/key"))},
		{"mixed provider node reference", credential.Resolver{NodeRoute: available, WorkspaceVault: available}, policyWithRoutes(route(proxycontract.DestinationProvider, "node-route://key"))},
		{"query", credential.Resolver{NodeRoute: available}, policyWithRoutes(route(proxycontract.DestinationLocalNode, "node-route://key?version=1"))},
		{"fragment", credential.Resolver{NodeRoute: available}, policyWithRoutes(route(proxycontract.DestinationLocalNode, "node-route://key#value"))},
		{"empty segment", credential.Resolver{NodeRoute: available}, policyWithRoutes(route(proxycontract.DestinationLocalNode, "node-route://team//key"))},
		{"traversal", credential.Resolver{NodeRoute: available}, policyWithRoutes(route(proxycontract.DestinationLocalNode, "node-route://team/../key"))},
		{"oversize reference", credential.Resolver{NodeRoute: available}, policyWithRoutes(route(proxycontract.DestinationLocalNode, "node-route://"+strings.Repeat("x", 300)))},
		{"unknown destination", credential.Resolver{WorkspaceVault: available}, policyWithRoutes(route(proxycontract.DestinationClass("unknown"), "workspace-vault://key"))},
		{"missing backend", credential.Resolver{}, policyWithRoutes(route(proxycontract.DestinationLocalNode, "node-route://key"))},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := testCase.resolver.Resolve(context.Background(), testCase.policy)
			var unavailable *credential.UnavailableError
			if !errors.Is(err, credential.ErrUnavailable) || !errors.As(err, &unavailable) {
				t.Fatalf("error is not typed unavailable: %v", err)
			}
		})
	}
}

func TestResolveValidatesCredentialShapeAndRedactsFailures(t *testing.T) {
	ref := "node-route://sensitive-reference"
	tests := []struct {
		name  string
		value []byte
	}{
		{"empty", []byte{}},
		{"newline", []byte("secret\nvalue")},
		{"carriage return", []byte("secret\rvalue")},
		{"nul", []byte("secret\x00value")},
		{"oversize", []byte(strings.Repeat("s", credential.MaxCredentialBytes+1))},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			secretBeforeZero := string(testCase.value)
			backend := &recordingBackend{values: map[string][]byte{ref: testCase.value}, calls: map[string]int{}}
			_, err := (credential.Resolver{NodeRoute: backend}).Resolve(context.Background(), policyWithRoutes(route(proxycontract.DestinationLocalNode, ref)))
			var unavailable *credential.UnavailableError
			if !errors.As(err, &unavailable) || unavailable.Kind != credential.FailureInvalidCredential {
				t.Fatalf("unexpected failure: %#v", err)
			}
			formatted := fmt.Sprintf("%v %#v", err, err)
			if strings.Contains(formatted, ref) || (secretBeforeZero != "" && strings.Contains(formatted, secretBeforeZero)) {
				t.Fatalf("failure formatting exposed sensitive input: %q", formatted)
			}
		})
	}
}

func TestResolveCancellationIsTypedAndPropagates(t *testing.T) {
	started := make(chan string, 1)
	never := make(chan struct{})
	backend := &recordingBackend{values: map[string][]byte{}, calls: map[string]int{}, started: started, release: never}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := (credential.Resolver{NodeRoute: backend}).Resolve(ctx, policyWithRoutes(route(proxycontract.DestinationLocalNode, "node-route://key")))
		done <- err
	}()
	<-started
	cancel()
	err := <-done
	var unavailable *credential.UnavailableError
	if !errors.Is(err, credential.ErrUnavailable) || !errors.Is(err, context.Canceled) || !errors.As(err, &unavailable) || unavailable.Kind != credential.FailureCancelled {
		t.Fatalf("cancellation classification was lost: %#v", err)
	}
}

func TestCredentialSnapshotDoesNotEnterResultEventOrStateFormatting(t *testing.T) {
	ref, secret := "workspace-vault://private/provider", "result-event-state-secret"
	backend := &recordingBackend{values: map[string][]byte{ref: []byte(secret)}, calls: map[string]int{}}
	snapshot, err := (credential.Resolver{WorkspaceVault: backend}).Resolve(context.Background(), policyWithRoutes(route(proxycontract.DestinationProvider, ref)))
	if err != nil {
		t.Fatal(err)
	}
	missingErr := func() error {
		_, lookupErr := snapshot.DestinationCredential(context.Background(), "workspace-vault://private/missing")
		return lookupErr
	}()
	artifacts := struct {
		Result   activation.Result
		Event    proxycontract.Event
		State    state.Reconciliation
		Provider credential.Provider
		Failure  string
	}{Provider: snapshot, Failure: fmt.Sprintf("%#v", missingErr)}
	formatted := fmt.Sprintf("%+v", artifacts)
	encoded, marshalErr := json.Marshal(artifacts)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	joined := formatted + string(encoded)
	if strings.Contains(joined, ref) || strings.Contains(joined, secret) {
		t.Fatalf("result/event/state artifacts exposed credential material: %s", joined)
	}
}

func policyWithRoutes(routes ...proxycontract.Route) proxycontract.Policy {
	return proxycontract.Policy{Routes: routes}
}

func route(class proxycontract.DestinationClass, ref string) proxycontract.Route {
	return proxycontract.Route{DestinationClass: class, CredentialRef: ref}
}

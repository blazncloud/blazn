//go:build darwin

package credential

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type recordingKeychainTransport struct {
	service string
	account string
	value   []byte
	status  int32
	err     error
	lookup  func(context.Context, string, string) ([]byte, int32, error)
}

func (t *recordingKeychainTransport) Lookup(ctx context.Context, service, account string) ([]byte, int32, error) {
	t.service, t.account = service, account
	if t.lookup != nil {
		return t.lookup(ctx, service, account)
	}
	return t.value, t.status, t.err
}

func TestDarwinKeychainBackendUsesDedicatedServiceAndCanonicalAccount(t *testing.T) {
	raw := []byte("secret-value")
	transport := &recordingKeychainTransport{value: raw}
	backend := newDarwinKeychainBackend(transport)
	wantRef := "workspace-vault://team/provider-key"
	got, err := backend.Lookup(context.Background(), wantRef)
	if err != nil || string(got) != "secret-value" {
		t.Fatalf("Lookup() value=%q err=%v", got, err)
	}
	if transport.service != "com.blazn.proxy.destination.v1alpha1" || transport.account != wantRef {
		t.Fatalf("lookup service=%q account=%q", transport.service, transport.account)
	}
	if transport.service == "com.blazn.cli.v1alpha1" {
		t.Fatal("proxy backend crossed into the authentication service namespace")
	}
	if !bytes.Equal(raw, make([]byte, len(raw))) {
		t.Fatal("transport buffer was not zeroed")
	}
	got[0] = 'X'
	if !bytes.Equal(raw, make([]byte, len(raw))) {
		t.Fatal("returned credential aliases the transport buffer")
	}
}

func TestDarwinKeychainBackendCannotReadAuthenticationService(t *testing.T) {
	type key struct{ service, account string }
	wantRef := "node-route://local/provider"
	items := map[key][]byte{
		{service: "com.blazn.cli.v1alpha1", account: wantRef}:                   []byte("authentication-secret"),
		{service: "com.blazn.proxy.destination.v1alpha1", account: wantRef}:     []byte("proxy-secret"),
		{service: "com.blazn.proxy.destination.v1alpha1", account: "other-ref"}: []byte("other-secret"),
	}
	transport := &recordingKeychainTransport{lookup: func(_ context.Context, service, account string) ([]byte, int32, error) {
		value, ok := items[key{service: service, account: account}]
		if !ok {
			return nil, errSecItemNotFound, nil
		}
		return append([]byte(nil), value...), errSecSuccess, nil
	}}
	got, err := newDarwinKeychainBackend(transport).Lookup(context.Background(), wantRef)
	if err != nil || string(got) != "proxy-secret" {
		t.Fatalf("Lookup() value=%q err=%v", got, err)
	}
}

func TestDarwinKeychainBackendFailureClassification(t *testing.T) {
	tests := []struct {
		name   string
		status int32
		err    error
		kind   KeychainFailureKind
	}{
		{name: "not found", status: errSecItemNotFound, kind: KeychainNotFound},
		{name: "interaction not allowed", status: errSecInteractionNotAllowed, kind: KeychainLockedOrDenied},
		{name: "authentication failed", status: errSecAuthFailed, kind: KeychainLockedOrDenied},
		{name: "user cancelled prompt", status: errSecUserCanceled, kind: KeychainLockedOrDenied},
		{name: "unknown status", status: -1, kind: KeychainBackendError},
		{name: "transport error", err: errors.New("sensitive native failure"), kind: KeychainBackendError},
		{name: "native bounds", err: errKeychainValueBounds, kind: KeychainInvalidValue},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := newDarwinKeychainBackend(&recordingKeychainTransport{value: []byte("sensitive"), status: test.status, err: test.err})
			_, err := backend.Lookup(context.Background(), "node-route://local/model")
			assertKeychainKind(t, err, test.kind)
		})
	}
}

func TestDarwinKeychainBackendRejectsInvalidReferencesWithoutLookup(t *testing.T) {
	invalid := []string{
		"", "node-route://", "NODE-route://local", "node-route://local//key",
		"node-route://local/key\nvalue", "node-route://local/key\x00value",
		strings.Repeat("a", maxReferenceBytes+1),
	}
	for _, ref := range invalid {
		t.Run(fmt.Sprintf("len-%d", len(ref)), func(t *testing.T) {
			transport := &recordingKeychainTransport{value: []byte("secret")}
			_, err := newDarwinKeychainBackend(transport).Lookup(context.Background(), ref)
			assertKeychainKind(t, err, KeychainInvalidInput)
			if transport.service != "" || transport.account != "" {
				t.Fatal("transport called for invalid reference")
			}
		})
	}
}

func TestDarwinKeychainBackendRejectsInvalidValuesAndZerosThem(t *testing.T) {
	tests := []struct {
		name  string
		value []byte
	}{
		{name: "empty", value: []byte{}},
		{name: "oversize", value: bytes.Repeat([]byte{'x'}, MaxCredentialBytes+1)},
		{name: "newline", value: []byte("secret\n")},
		{name: "carriage return", value: []byte("secret\r")},
		{name: "nul", value: []byte("secret\x00value")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := &recordingKeychainTransport{value: test.value}
			_, err := newDarwinKeychainBackend(transport).Lookup(context.Background(), "workspace-vault://team/key")
			assertKeychainKind(t, err, KeychainInvalidValue)
			if !bytes.Equal(test.value, make([]byte, len(test.value))) {
				t.Fatal("invalid transport buffer was not zeroed")
			}
		})
	}
}

func TestDarwinKeychainBackendCancellation(t *testing.T) {
	t.Run("before lookup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		transport := &recordingKeychainTransport{value: []byte("secret")}
		_, err := newDarwinKeychainBackend(transport).Lookup(ctx, "node-route://local/key")
		assertKeychainKind(t, err, KeychainCancelled)
		if transport.service != "" {
			t.Fatal("transport called after cancellation")
		}
	})

	t.Run("during lookup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		raw := []byte("secret")
		transport := &recordingKeychainTransport{lookup: func(context.Context, string, string) ([]byte, int32, error) {
			cancel()
			return raw, errSecSuccess, nil
		}}
		_, err := newDarwinKeychainBackend(transport).Lookup(ctx, "node-route://local/key")
		assertKeychainKind(t, err, KeychainCancelled)
		if !bytes.Equal(raw, make([]byte, len(raw))) {
			t.Fatal("buffer returned during cancellation was not zeroed")
		}
	})
}

func TestDarwinKeychainErrorRedactsEveryFormattingSurface(t *testing.T) {
	err := &KeychainError{Kind: KeychainBackendError}
	outputs := []string{err.Error(), err.String(), fmt.Sprint(err), fmt.Sprintf("%s", err), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)}
	encoded, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	outputs = append(outputs, string(encoded))
	for _, output := range outputs {
		for _, forbidden := range []string{"workspace-vault://", "node-route://", "secret", "native failure", "backend_error"} {
			if strings.Contains(output, forbidden) {
				t.Fatalf("formatted error leaked %q: %q", forbidden, output)
			}
		}
	}
}

func assertKeychainKind(t *testing.T, err error, want KeychainFailureKind) {
	t.Helper()
	var keychainErr *KeychainError
	if !errors.As(err, &keychainErr) {
		t.Fatalf("error=%#v is not a KeychainError", err)
	}
	if keychainErr.Kind != want {
		t.Fatalf("error=%#v kind=%q, want %q", err, keychainErr.Kind, want)
	}
}

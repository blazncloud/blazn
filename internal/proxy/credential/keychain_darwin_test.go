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
	"unsafe"
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
		{name: "interaction required", status: errSecInteractionRequired, kind: KeychainLockedOrDenied},
		{name: "authentication failed", status: errSecAuthFailed, kind: KeychainLockedOrDenied},
		{name: "user cancelled prompt", status: errSecUserCanceled, kind: KeychainCancelled},
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

func TestLookupDarwinKeychainUsesDefaultAndScrubsBeforeFree(t *testing.T) {
	const (
		defaultKeychain = uintptr(0x101)
		itemRef         = uintptr(0x202)
		service         = "com.blazn.proxy.destination.v1alpha1"
		account         = "workspace-vault://team/provider-key"
	)
	tests := []struct {
		name         string
		raw          []byte
		status       int32
		cancelInFind bool
		wantStatus   int32
		wantErr      error
		wantValue    string
	}{
		{name: "success", raw: []byte("secret-value"), wantValue: "secret-value"},
		{name: "cancel after allocation", raw: []byte("secret-value"), cancelInFind: true, wantErr: context.Canceled},
		{name: "oversize", raw: bytes.Repeat([]byte{'x'}, MaxCredentialBytes+1), wantErr: errKeychainValueBounds},
		{name: "native status failure", raw: []byte("secret-value"), status: errSecItemNotFound, wantStatus: errSecItemNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			events := []string{}
			native := darwinKeychainNative{
				copyDefault: func(out *uintptr) int32 {
					events = append(events, "copy-default")
					*out = defaultKeychain
					return errSecSuccess
				},
				find: func(keychain uintptr, serviceLength uint32, _ uintptr, accountLength uint32, _ uintptr, length *uint32, data *unsafe.Pointer, item *uintptr) int32 {
					events = append(events, "find")
					if keychain != defaultKeychain {
						t.Fatalf("find keychain=%#x, want explicit default %#x", keychain, defaultKeychain)
					}
					if serviceLength != uint32(len(service)) {
						t.Fatalf("find service length=%d, want %d", serviceLength, len(service))
					}
					if accountLength != uint32(len(account)) {
						t.Fatalf("find account length=%d, want %d", accountLength, len(account))
					}
					*length = uint32(len(test.raw))
					if len(test.raw) > 0 {
						*data = unsafe.Pointer(unsafe.SliceData(test.raw))
					}
					*item = itemRef
					if test.cancelInFind {
						cancel()
					}
					return test.status
				},
				freeContent: func(attributes uintptr, data unsafe.Pointer) int32 {
					events = append(events, "free")
					if attributes != 0 || data != unsafe.Pointer(unsafe.SliceData(test.raw)) {
						t.Fatalf("free arguments attributes=%#x data=%p", attributes, data)
					}
					if !bytes.Equal(test.raw, make([]byte, len(test.raw))) {
						t.Fatal("native password allocation was not scrubbed before free")
					}
					return errSecSuccess
				},
				release: func(ref uintptr) {
					events = append(events, fmt.Sprintf("release-%#x", ref))
				},
			}

			got, status, err := lookupDarwinKeychain(ctx, service, account, native)
			if status != test.wantStatus || !errors.Is(err, test.wantErr) || string(got) != test.wantValue {
				t.Fatalf("lookup value=%q status=%d err=%v, want value=%q status=%d err=%v", got, status, err, test.wantValue, test.wantStatus, test.wantErr)
			}
			wantEvents := []string{"copy-default", "find", "free", "release-0x202", "release-0x101"}
			if fmt.Sprint(events) != fmt.Sprint(wantEvents) {
				t.Fatalf("native events=%v, want %v", events, wantEvents)
			}
		})
	}
}

func TestLookupDarwinKeychainReleasesDefaultReferenceOnCopyFailure(t *testing.T) {
	const defaultKeychain = uintptr(0x101)
	released := uintptr(0)
	native := darwinKeychainNative{
		copyDefault: func(out *uintptr) int32 {
			*out = defaultKeychain
			return -1
		},
		release: func(ref uintptr) { released = ref },
	}
	value, status, err := lookupDarwinKeychain(context.Background(), "service", "account", native)
	if value != nil || status != -1 || err != nil {
		t.Fatalf("lookup value=%q status=%d err=%v", value, status, err)
	}
	if released != defaultKeychain {
		t.Fatalf("released=%#x, want default Keychain %#x", released, defaultKeychain)
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

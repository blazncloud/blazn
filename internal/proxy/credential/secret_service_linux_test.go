//go:build linux

package credential

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testSecretRef     = "workspace-vault://team/provider"
	testSecretSession = "/org/freedesktop/secrets/session/s1"
	testSecretPath    = "/org/freedesktop/secrets/collection/login/item1"
)

type fakeSecretServiceTransport struct {
	mu       sync.Mutex
	requests []secretServiceRequest
	response secretServiceResponse
	err      error
	wait     bool
}

func (transport *fakeSecretServiceTransport) Lookup(ctx context.Context, request secretServiceRequest) (secretServiceResponse, error) {
	transport.mu.Lock()
	transport.requests = append(transport.requests, request)
	transport.mu.Unlock()
	if transport.wait {
		<-ctx.Done()
		return secretServiceResponse{}, ctx.Err()
	}
	return transport.response, transport.err
}

func successfulSecretResponse(uid int, value []byte) secretServiceResponse {
	return secretServiceResponse{
		OwnerUID: uid,
		Session:  testSecretSession,
		Items: []secretServiceItem{{
			Path:       testSecretPath,
			Session:    testSecretSession,
			Attributes: map[string]string{"service": secretNamespace, "account": testSecretRef},
			Value:      value,
		}},
	}
}

func testSecretServiceBackend(uid int, transport secretServiceTransport) *SecretServiceBackend {
	return &SecretServiceBackend{uid: uid, timeout: time.Second, transport: transport}
}

func TestSecretServiceUsesFixedBusNamespaceAndCanonicalAccount(t *testing.T) {
	const uid = 1801
	secret := []byte("destination-secret")
	transport := &fakeSecretServiceTransport{response: successfulSecretResponse(uid, secret)}
	backend := testSecretServiceBackend(uid, transport)
	for _, variable := range []string{"HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		t.Setenv(variable, "/attacker/controlled")
	}
	value, err := backend.Lookup(context.Background(), testSecretRef)
	if err != nil || string(value) != "destination-secret" {
		t.Fatalf("lookup failed: value=%q error=%v", value, err)
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.requests) != 1 {
		t.Fatalf("transport requests=%d, want 1", len(transport.requests))
	}
	want := secretServiceRequest{BusPath: "/run/user/1801/bus", UID: uid, Service: secretNamespace, Account: testSecretRef}
	if got := transport.requests[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("request=%#v, want %#v", got, want)
	}
	if secretNamespace != "com.blazn.proxy.destination.v1alpha1" {
		t.Fatalf("service namespace changed: %q", secretNamespace)
	}
	for _, current := range secret {
		if current != 0 {
			t.Fatal("transport-owned secret buffer was not zeroed")
		}
	}
}

func TestSecretServiceFailsClosedForMissingLockedDuplicateAndEmpty(t *testing.T) {
	const uid = 1802
	tests := []struct {
		name     string
		response secretServiceResponse
		kind     FailureKind
	}{
		{"missing", secretServiceResponse{OwnerUID: uid, Session: testSecretSession}, FailureBackendUnavailable},
		{"locked", func() secretServiceResponse {
			response := successfulSecretResponse(uid, []byte("locked-secret"))
			response.Items[0].Locked = true
			return response
		}(), FailureBackendUnavailable},
		{"duplicate", func() secretServiceResponse {
			response := successfulSecretResponse(uid, []byte("first-secret"))
			response.Items = append(response.Items, secretServiceItem{Path: "/org/freedesktop/secrets/collection/login/item2", Session: testSecretSession, Attributes: map[string]string{"service": secretNamespace, "account": testSecretRef}, Value: []byte("second-secret")})
			return response
		}(), FailureBackendUnavailable},
		{"empty", successfulSecretResponse(uid, []byte{}), FailureInvalidCredential},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			backend := testSecretServiceBackend(uid, &fakeSecretServiceTransport{response: testCase.response})
			value, err := backend.Lookup(context.Background(), testSecretRef)
			var unavailable *UnavailableError
			if value != nil || !errors.Is(err, ErrUnavailable) || !errors.As(err, &unavailable) || unavailable.Kind != testCase.kind {
				t.Fatalf("value=%q error=%#v", value, err)
			}
			for _, item := range testCase.response.Items {
				for _, current := range item.Value {
					if current != 0 {
						t.Fatal("rejected secret buffer was not zeroed")
					}
				}
			}
		})
	}
}

func TestSecretServiceRejectsUnavailableWrongOwnerSessionAndAttributes(t *testing.T) {
	const uid = 1803
	tests := []struct {
		name      string
		response  secretServiceResponse
		transport error
	}{
		{"unavailable", secretServiceResponse{}, errors.New("transport-secret")},
		{"wrong owner", successfulSecretResponse(uid+1, []byte("owner-secret")), nil},
		{"invalid session", func() secretServiceResponse {
			response := successfulSecretResponse(uid, []byte("session-secret"))
			response.Session = "/wrong/session"
			return response
		}(), nil},
		{"wrong item session", func() secretServiceResponse {
			response := successfulSecretResponse(uid, []byte("item-session-secret"))
			response.Items[0].Session = "/org/freedesktop/secrets/session/other"
			return response
		}(), nil},
		{"wrong service", func() secretServiceResponse {
			response := successfulSecretResponse(uid, []byte("service-secret"))
			response.Items[0].Attributes["service"] = "attacker.service"
			return response
		}(), nil},
		{"wrong account", func() secretServiceResponse {
			response := successfulSecretResponse(uid, []byte("account-secret"))
			response.Items[0].Attributes["account"] = "workspace-vault://other"
			return response
		}(), nil},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			backend := testSecretServiceBackend(uid, &fakeSecretServiceTransport{response: testCase.response, err: testCase.transport})
			value, err := backend.Lookup(context.Background(), testSecretRef)
			var unavailable *UnavailableError
			if value != nil || !errors.As(err, &unavailable) || unavailable.Kind != FailureBackendUnavailable {
				t.Fatalf("value=%q error=%#v", value, err)
			}
		})
	}
}

func TestSecretServiceTimeoutAndCancellationAreTyped(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		backend := testSecretServiceBackend(1804, &fakeSecretServiceTransport{wait: true})
		backend.timeout = 10 * time.Millisecond
		_, err := backend.Lookup(context.Background(), testSecretRef)
		var unavailable *UnavailableError
		if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &unavailable) || unavailable.Kind != FailureCancelled {
			t.Fatalf("timeout error=%#v", err)
		}
	})
	t.Run("caller cancellation", func(t *testing.T) {
		backend := testSecretServiceBackend(1805, &fakeSecretServiceTransport{wait: true})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := backend.Lookup(ctx, testSecretRef)
		var unavailable *UnavailableError
		if !errors.Is(err, context.Canceled) || !errors.As(err, &unavailable) || unavailable.Kind != FailureCancelled {
			t.Fatalf("cancellation error=%#v", err)
		}
	})
}

func TestDBusSecretServiceAuthAndHelloStallsHonorContext(t *testing.T) {
	t.Run("auth", func(t *testing.T) {
		path, accepted, done := startSecretServiceSocketFixture(t, func(connection *net.UnixConn) error {
			_, _ = io.Copy(io.Discard, connection)
			return nil
		})
		assertSecretServiceSocketStallCancelled(t, path, accepted, done, nil)
	})

	t.Run("hello", func(t *testing.T) {
		helloStarted := make(chan struct{})
		path, accepted, done := startSecretServiceSocketFixture(t, func(connection *net.UnixConn) error {
			reader := bufio.NewReader(connection)
			prefix, err := reader.ReadByte()
			if err != nil || prefix != 0 {
				return fmt.Errorf("read auth prefix: byte=%d error=%v", prefix, err)
			}
			line, err := reader.ReadString('\n')
			if err != nil || strings.TrimSpace(line) != "AUTH" {
				return fmt.Errorf("read auth offer: line=%q error=%v", line, err)
			}
			if _, err := io.WriteString(connection, "REJECTED EXTERNAL\r\n"); err != nil {
				return err
			}
			line, err = reader.ReadString('\n')
			if err != nil || strings.TrimSpace(line) != "AUTH EXTERNAL" {
				return fmt.Errorf("read external auth: line=%q error=%v", line, err)
			}
			if _, err := io.WriteString(connection, "OK 0123456789abcdef0123456789abcdef\r\n"); err != nil {
				return err
			}
			line, err = reader.ReadString('\n')
			if err != nil {
				return err
			}
			if strings.TrimSpace(line) == "NEGOTIATE_UNIX_FD" {
				if _, err := io.WriteString(connection, "ERROR unsupported\r\n"); err != nil {
					return err
				}
				line, err = reader.ReadString('\n')
				if err != nil {
					return err
				}
			}
			if strings.TrimSpace(line) != "BEGIN" {
				return fmt.Errorf("read auth completion: line=%q", line)
			}
			close(helloStarted)
			_, _ = io.Copy(io.Discard, connection)
			return nil
		})
		assertSecretServiceSocketStallCancelled(t, path, accepted, done, helloStarted)
	})
}

func startSecretServiceSocketFixture(t *testing.T, serve func(*net.UnixConn) error) (string, <-chan struct{}, <-chan error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bus")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			done <- acceptErr
			return
		}
		close(accepted)
		defer connection.Close()
		done <- serve(connection)
	}()
	return path, accepted, done
}

func assertSecretServiceSocketStallCancelled(t *testing.T, path string, accepted <-chan struct{}, done <-chan error, stage <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := lookupSecretServiceSocket(ctx, secretServiceRequest{BusPath: path, UID: os.Getuid()})
		result <- err
	}()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("fixture did not accept the same-UID connection before timeout")
	}
	if stage != nil {
		select {
		case <-stage:
		case serveErr := <-done:
			t.Fatalf("fixture stopped before the requested D-Bus stage: %v", serveErr)
		case <-time.After(time.Second):
			t.Fatal("fixture did not reach the requested D-Bus stage before timeout")
		}
	}
	started := time.Now()
	cancel()
	select {
	case err := <-result:
		if err != errSecretServiceTransport || !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("stalled lookup error=%v context=%v", err, ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("stalled lookup did not return after cancellation")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stalled lookup exceeded cancellation bound: %s", elapsed)
	}
	select {
	case serveErr := <-done:
		if serveErr != nil {
			t.Fatalf("fixture server error: %v", serveErr)
		}
	case <-time.After(time.Second):
		t.Fatal("fixture connection was not closed after cancellation")
	}
}

func TestSecretServiceHasNoFallbackOrPublicMutationSurface(t *testing.T) {
	backendType := reflect.TypeOf(SecretServiceBackend{})
	for index := 0; index < backendType.NumField(); index++ {
		name := strings.ToLower(backendType.Field(index).Name)
		if strings.Contains(name, "fallback") || strings.Contains(name, "command") || strings.Contains(name, "file") {
			t.Fatalf("backend gained alternate authority field %q", name)
		}
	}
	typeOfBackend := reflect.TypeOf(NewSecretServiceBackend())
	for _, forbidden := range []string{"Put", "Store", "Delete", "Clear", "Unlock"} {
		if _, exists := typeOfBackend.MethodByName(forbidden); exists {
			t.Fatalf("backend exposes mutation method %s", forbidden)
		}
	}
}

func TestSecretServiceFormattingAndArtifactsRedactSensitiveMaterial(t *testing.T) {
	const uid = 1806
	secret := "formatting-destination-secret"
	parameters := "formatting-secret-parameters"
	response := successfulSecretResponse(uid, []byte(secret))
	wireValue := secretValue{Session: testSecretSession, Parameters: []byte(parameters), Value: []byte(secret), ContentType: "sensitive/content-type"}
	transport := &fakeSecretServiceTransport{response: response, err: errors.New("transport-secret")}
	backend := testSecretServiceBackend(uid, transport)
	_, err := backend.Lookup(context.Background(), testSecretRef)
	joined := fmt.Sprintf("%v %#v %v %#v %v %#v %v %#v", backend, backend, err, err, response, response, wireValue, wireValue)
	encoded, marshalErr := json.Marshal(struct {
		Backend  *SecretServiceBackend
		Response secretServiceResponse
		Value    secretValue
		Error    string
	}{Backend: backend, Response: response, Value: wireValue, Error: fmt.Sprintf("%#v", err)})
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	joined += string(encoded)
	wireEncoded, marshalErr := json.Marshal(wireValue)
	if marshalErr != nil || string(wireEncoded) != `"[REDACTED secret value]"` {
		t.Fatalf("secret value JSON=%s error=%v", wireEncoded, marshalErr)
	}
	for _, forbidden := range []string{secret, parameters, "sensitive/content-type", "transport-secret", testSecretRef, testSecretSession, testSecretPath} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("formatted artifacts exposed sensitive material %q: %s", forbidden, joined)
		}
	}
	if strings.Contains(fmt.Sprintf("%#v", transport.requests[0]), testSecretRef) {
		t.Fatal("request GoString exposed canonical credential reference")
	}
}

func TestProductionSecretServiceBackendUsesOSUIDOnly(t *testing.T) {
	t.Setenv("HOME", "/attacker/home")
	t.Setenv("XDG_RUNTIME_DIR", "/attacker/runtime")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/attacker/bus")
	backend := NewSecretServiceBackend()
	if backend.uid != os.Getuid() || backend.timeout != secretLookupTimeout {
		t.Fatalf("production backend uid=%d timeout=%s", backend.uid, backend.timeout)
	}
	if _, ok := backend.transport.(dbusSecretServiceTransport); !ok {
		t.Fatalf("production transport=%T", backend.transport)
	}
}

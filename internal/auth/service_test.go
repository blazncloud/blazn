package auth

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/KingJammin/blazn/internal/client"
)

type memoryStore struct {
	value   []byte
	put     int
	deleted int
	err     error
}

func (s *memoryStore) Get() ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	if len(s.value) == 0 {
		return nil, ErrNotFound
	}
	return append([]byte(nil), s.value...), nil
}
func (s *memoryStore) Put(value []byte) error {
	s.put++
	s.value = append([]byte(nil), value...)
	return nil
}
func (s *memoryStore) Delete() error       { s.deleted++; s.value = nil; return nil }
func (s *memoryStore) Description() string { return "test store" }

type fakeAPI struct {
	authorization        client.DeviceAuthorization
	session              client.Session
	current              client.CurrentUser
	devices              client.DeviceList
	exchangeErrs         []error
	refreshes            int
	deleted              int
	deletedToken         string
	revoked              string
	authorizationRequest client.DeviceAuthorizationRequest
	sessionRequest       client.DeviceSessionRequest
	refreshRequest       client.RefreshSessionRequest
}

func (a *fakeAPI) CreateDeviceAuthorization(_ context.Context, request client.DeviceAuthorizationRequest) (client.DeviceAuthorization, error) {
	a.authorizationRequest = request
	return a.authorization, nil
}
func (a *fakeAPI) ExchangeDeviceAuthorization(_ context.Context, request client.DeviceSessionRequest) (client.Session, error) {
	a.sessionRequest = request
	if len(a.exchangeErrs) > 0 {
		err := a.exchangeErrs[0]
		a.exchangeErrs = a.exchangeErrs[1:]
		return client.Session{}, err
	}
	return a.session, nil
}
func (a *fakeAPI) RefreshSession(_ context.Context, request client.RefreshSessionRequest) (client.Session, error) {
	a.refreshes++
	a.refreshRequest = request
	return a.session, nil
}
func (a *fakeAPI) DeleteCurrentSession(_ context.Context, token string) error {
	a.deleted++
	a.deletedToken = token
	return nil
}
func (a *fakeAPI) GetCurrentUser(context.Context, string) (client.CurrentUser, error) {
	return a.current, nil
}
func (a *fakeAPI) ListDevices(context.Context, string) (client.DeviceList, error) {
	return a.devices, nil
}
func (a *fakeAPI) RevokeDevice(_ context.Context, _ string, id string) error {
	a.revoked = id
	return nil
}

func testService(api *fakeAPI, store *memoryStore) *Service {
	service := NewService(api, store)
	service.now = func() time.Time { return time.Unix(1_800_000_000, 0).UTC() }
	service.sleep = func(context.Context, time.Duration) error { return nil }
	return service
}

func storedCredentials(t *testing.T, expiresAt string) []byte {
	t.Helper()
	encoded, err := json.Marshal(Credentials{AccessToken: "old", RefreshToken: "refresh", DeviceID: "dev-1", ExpiresAt: mustTime(t, expiresAt), DevicePrivateKey: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.PrivateKeySize))})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestDeviceLoginPollsPendingStoresAndVerifies(t *testing.T) {
	api := &fakeAPI{
		authorization: client.DeviceAuthorization{DeviceCode: "device-secret", UserCode: "ABCD-EFGH", VerificationURI: "https://login.example/device", Challenge: "server-challenge", ExpiresIn: 600, Interval: 1},
		session:       client.Session{AccessToken: "access-secret", RefreshToken: "refresh-secret", ExpiresIn: 300, DeviceID: "dev-1"},
		current:       client.CurrentUser{User: client.User{ID: "user-1", DisplayName: "Blaze"}, Device: client.Device{ID: "dev-1", Name: "laptop"}},
		exchangeErrs:  []error{&client.APIError{StatusCode: http.StatusPreconditionRequired, Body: client.ErrorBody{Code: "authorization_pending"}}},
	}
	store := &memoryStore{}
	service := testService(api, store)
	start, code, interval, err := service.BeginLogin(context.Background())
	if err != nil || start.UserCode != "ABCD-EFGH" || code != "device-secret" || interval != time.Second {
		t.Fatalf("BeginLogin = %#v %q %v %v", start, code, interval, err)
	}
	result, err := service.CompleteLogin(context.Background(), code, interval)
	if err != nil || result.User.ID != "user-1" || store.put != 1 {
		t.Fatalf("CompleteLogin = %#v store=%#v err=%v", result, store, err)
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(api.authorizationRequest.DevicePublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize || api.sessionRequest.DeviceCode != "device-secret" {
		t.Fatalf("device proof request = %#v public-key-size=%d err=%v", api.sessionRequest, len(publicKey), err)
	}
	proof, err := base64.RawURLEncoding.DecodeString(api.sessionRequest.Proof)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(publicKey), []byte(deviceProofPayload("device-secret", "server-challenge")), proof) {
		t.Fatal("device proof did not verify")
	}
	if string(store.value) == "" || string(store.value) == "access-secret" {
		t.Fatalf("stored value is not structured credentials: %q", store.value)
	}
}

func TestStatusRefreshesExpiredSession(t *testing.T) {
	api := &fakeAPI{
		session: client.Session{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresIn: 300, DeviceID: "dev-1"},
		current: client.CurrentUser{User: client.User{ID: "user-1"}, Device: client.Device{ID: "dev-1"}},
	}
	store := &memoryStore{value: storedCredentials(t, "2026-01-01T00:00:00Z")}
	service := testService(api, store)
	status, err := service.Status(context.Background())
	if err != nil || !status.Authenticated || api.refreshes != 1 || store.put != 1 {
		t.Fatalf("Status = %#v refreshes=%d puts=%d err=%v", status, api.refreshes, store.put, err)
	}
	if api.refreshRequest.DeviceID != "dev-1" || api.refreshRequest.RefreshToken != "refresh" || api.refreshRequest.Proof == "" {
		t.Fatalf("refresh request = %#v", api.refreshRequest)
	}
}

func TestStatusWithoutCredentialsIsNotAnError(t *testing.T) {
	service := testService(&fakeAPI{}, &memoryStore{})
	status, err := service.Status(context.Background())
	if err != nil || status.Authenticated || status.Store != "test store" {
		t.Fatalf("Status = %#v err=%v", status, err)
	}
}

func TestLogoutRevokesThenDeletes(t *testing.T) {
	api := &fakeAPI{}
	store := &memoryStore{value: storedCredentials(t, "2030-01-01T00:00:00Z")}
	result, err := testService(api, store).Logout(context.Background())
	if err != nil || result.Status != "logged_out" || api.deleted != 1 || store.deleted != 1 {
		t.Fatalf("Logout = %#v api=%#v store=%#v err=%v", result, api, store, err)
	}
}

func TestLogoutRefreshesExpiredAccessBeforeRemoteRevocation(t *testing.T) {
	api := &fakeAPI{session: client.Session{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", ExpiresIn: 300, DeviceID: "dev-1"}}
	store := &memoryStore{value: storedCredentials(t, "2026-01-01T00:00:00Z")}
	result, err := testService(api, store).Logout(context.Background())
	if err != nil || result.Status != "logged_out" || api.refreshes != 1 || api.deletedToken != "fresh-access" || store.deleted != 1 {
		t.Fatalf("Logout = %#v api=%#v store=%#v err=%v", result, api, store, err)
	}
}

func TestRevokeCurrentDeviceDeletesLocalSession(t *testing.T) {
	api := &fakeAPI{}
	store := &memoryStore{value: storedCredentials(t, "2030-01-01T00:00:00Z")}
	if err := testService(api, store).RevokeDevice(context.Background(), "dev-1"); err != nil {
		t.Fatal(err)
	}
	if api.revoked != "dev-1" || store.deleted != 1 {
		t.Fatalf("api=%#v store=%#v", api, store)
	}
}

func TestIncompleteSessionNeverReplacesCredential(t *testing.T) {
	api := &fakeAPI{session: client.Session{AccessToken: "access"}}
	store := &memoryStore{}
	service := testService(api, store)
	service.pendingPrivateKey = make([]byte, ed25519.PrivateKeySize)
	service.pendingChallenge = "challenge"
	_, err := service.CompleteLogin(context.Background(), "code", time.Second)
	if err == nil || store.put != 0 {
		t.Fatalf("err=%v puts=%d", err, store.put)
	}
}

func TestLoginContextCancellationStopsPolling(t *testing.T) {
	api := &fakeAPI{exchangeErrs: []error{&client.APIError{StatusCode: http.StatusPreconditionRequired, Body: client.ErrorBody{Code: "authorization_pending"}}}}
	service := testService(api, &memoryStore{})
	service.pendingPrivateKey = make([]byte, ed25519.PrivateKeySize)
	service.pendingChallenge = "challenge"
	service.sleep = func(context.Context, time.Duration) error { return context.Canceled }
	_, err := service.CompleteLogin(context.Background(), "code", time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestAuthAPIRequiresTLSExceptExplicitLoopback(t *testing.T) {
	if err := validateAuthAPIURL("http://example.com"); err == nil {
		t.Fatal("cleartext remote API was accepted")
	}
	t.Setenv("BLAZN_ALLOW_INSECURE_LOCALHOST", "1")
	if err := validateAuthAPIURL("http://127.0.0.1:8080"); err != nil {
		t.Fatalf("explicit loopback API rejected: %v", err)
	}
}

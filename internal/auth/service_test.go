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
	value     []byte
	put       int
	deleted   int
	err       error
	putErrs   []error
	deleteErr error
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
	if len(s.putErrs) > 0 {
		err := s.putErrs[0]
		s.putErrs = s.putErrs[1:]
		if err != nil {
			return err
		}
	}
	s.value = append([]byte(nil), value...)
	return nil
}
func (s *memoryStore) Delete() error {
	s.deleted++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.value = nil
	return nil
}
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
	deleteErr            error
	revoked              string
	authorizationRequest client.DeviceAuthorizationRequest
	sessionRequest       client.DeviceSessionRequest
	refreshRequest       client.RefreshSessionRequest
	refreshErr           error
	currentErr           error
	currentErrs          []error
	currentCalls         int
	revokeSessionRequest client.RefreshSessionRequest
	revokeSessionErr     error
	revokeSessions       int
	stableRevokeRequest  client.RevokeSessionRequest
	stableRevokeErr      error
	stableRevokes        int
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
	if a.refreshErr != nil {
		return client.Session{}, a.refreshErr
	}
	return a.session, nil
}
func (a *fakeAPI) DeleteCurrentSession(_ context.Context, token string) error {
	a.deleted++
	a.deletedToken = token
	return a.deleteErr
}
func (a *fakeAPI) GetCurrentUser(context.Context, string) (client.CurrentUser, error) {
	a.currentCalls++
	if len(a.currentErrs) > 0 {
		err := a.currentErrs[0]
		a.currentErrs = a.currentErrs[1:]
		return a.current, err
	}
	return a.current, a.currentErr
}
func (a *fakeAPI) RevokeSession(_ context.Context, request client.RevokeSessionRequest) error {
	a.stableRevokes++
	a.stableRevokeRequest = request
	return a.stableRevokeErr
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
	encoded, err := json.Marshal(Credentials{APIOrigin: defaultAPIURL, AccessToken: "old", RefreshToken: "refresh", DeviceID: "dev-1", ExpiresAt: mustTime(t, expiresAt), DevicePrivateKey: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.PrivateKeySize))})
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

func TestRefreshStoreFailureRevokesRotatedSessionAndRemovesStaleCredential(t *testing.T) {
	api := &fakeAPI{session: client.Session{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresIn: 300, DeviceID: "dev-1"}}
	store := &memoryStore{value: storedCredentials(t, "2026-01-01T00:00:00Z"), putErrs: []error{errors.New("keyring unavailable")}}
	_, err := testService(api, store).Status(context.Background())
	if err == nil || api.deletedToken != "new-access" || store.deleted != 1 || len(store.value) != 0 {
		t.Fatalf("api=%#v store=%#v err=%v", api, store, err)
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
	if err != nil || result.Status != "logged_out" || api.stableRevokes != 1 || api.revokeSessions != 0 || api.deleted != 0 || store.deleted != 1 {
		t.Fatalf("Logout = %#v api=%#v store=%#v err=%v", result, api, store, err)
	}
}

func TestLogoutRefreshesExpiredAccessBeforeRemoteRevocation(t *testing.T) {
	api := &fakeAPI{session: client.Session{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", ExpiresIn: 300, DeviceID: "dev-1"}}
	store := &memoryStore{value: storedCredentials(t, "2026-01-01T00:00:00Z")}
	result, err := testService(api, store).Logout(context.Background())
	if err != nil || result.Status != "logged_out" || api.refreshes != 1 || api.stableRevokes != 1 || api.stableRevokeRequest.DeviceID != "dev-1" || store.deleted != 1 {
		t.Fatalf("Logout = %#v api=%#v store=%#v err=%v", result, api, store, err)
	}
}

func TestInvalidRefreshIsTerminalAndDeletesLocalCredential(t *testing.T) {
	api := &fakeAPI{refreshErr: &client.APIError{StatusCode: http.StatusUnauthorized, Body: client.ErrorBody{Code: "refresh_invalid"}}}
	store := &memoryStore{value: storedCredentials(t, "2026-01-01T00:00:00Z")}
	status, err := testService(api, store).Status(context.Background())
	if err != nil || status.Authenticated || store.deleted != 1 {
		t.Fatalf("Status=%#v store=%#v err=%v", status, store, err)
	}
}

func TestLogoutPreservesLocalCredentialWhenExpiredSessionCannotRefresh(t *testing.T) {
	api := &fakeAPI{refreshErr: errors.New("network unavailable")}
	store := &memoryStore{value: storedCredentials(t, "2026-01-01T00:00:00Z")}
	result, err := testService(api, store).Logout(context.Background())
	if err == nil || result.Status != "logout_failed" || result.RemoteRevoked || store.deleted != 0 {
		t.Fatalf("Logout=%#v store=%#v err=%v", result, store, err)
	}
}

func TestLogoutPreservesLocalCredentialWhenRemoteRevocationFails(t *testing.T) {
	api := &fakeAPI{stableRevokeErr: errors.New("network unavailable"), revokeSessionErr: errors.New("network unavailable")}
	store := &memoryStore{value: storedCredentials(t, "2030-01-01T00:00:00Z")}
	result, err := testService(api, store).Logout(context.Background())
	if err == nil || result.Status != "logout_failed" || result.RemoteRevoked || store.deleted != 0 {
		t.Fatalf("Logout=%#v store=%#v err=%v", result, store, err)
	}
}

func TestLogoutUsesProofBoundRevocationWhenAccessRevocationIsAmbiguous(t *testing.T) {
	api := &fakeAPI{deleteErr: &client.APIError{StatusCode: http.StatusUnauthorized, Body: client.ErrorBody{Code: "session_revoked"}}}
	store := &memoryStore{value: storedCredentials(t, "2030-01-01T00:00:00Z")}
	result, err := testService(api, store).Logout(context.Background())
	if err != nil || !result.RemoteRevoked || api.stableRevokes != 1 || api.revokeSessions != 0 || store.deleted != 1 {
		t.Fatalf("Logout=%#v api=%#v store=%#v err=%v", result, api, store, err)
	}
}

func TestOriginMismatchRefusesBeforeAnyAPIUse(t *testing.T) {
	api := &fakeAPI{}
	store := &memoryStore{value: storedCredentials(t, "2030-01-01T00:00:00Z")}
	service := newService(api, store, "https://different.example", noopCredentialLocker{})
	_, err := service.Status(context.Background())
	if !errors.Is(err, ErrOriginMismatch) || api.currentCalls != 0 || api.refreshes != 0 || api.deleted != 0 {
		t.Fatalf("err=%v api=%#v", err, api)
	}
}

func TestPostExchangeVerificationFailureRevokesAndDeletes(t *testing.T) {
	api := &fakeAPI{currentErr: errors.New("verification unavailable")}
	store := &memoryStore{}
	service := testService(api, store)
	privateKey := make(ed25519.PrivateKey, ed25519.PrivateKeySize)
	_, err := service.finishLogin(context.Background(), client.Session{AccessToken: "access", RefreshToken: "refresh", DeviceID: "dev-1", ExpiresIn: 300}, privateKey)
	if err == nil || api.deleted != 1 || store.deleted != 1 || len(store.value) != 0 {
		t.Fatalf("api=%#v store=%#v err=%v", api, store, err)
	}
}

func TestPostExchangeCleanupFailureRetainsCredential(t *testing.T) {
	api := &fakeAPI{currentErr: errors.New("verification unavailable"), deleteErr: errors.New("network unavailable"), stableRevokeErr: errors.New("network unavailable"), revokeSessionErr: errors.New("network unavailable")}
	store := &memoryStore{}
	service := testService(api, store)
	privateKey := make(ed25519.PrivateKey, ed25519.PrivateKeySize)
	_, err := service.finishLogin(context.Background(), client.Session{AccessToken: "access", RefreshToken: "refresh", DeviceID: "dev-1", ExpiresIn: 300}, privateKey)
	if err == nil || len(store.value) == 0 || store.deleted != 0 {
		t.Fatalf("api=%#v store=%#v err=%v", api, store, err)
	}
}

func TestPostExchangeStoreFailureAttemptsCleanupAndRetainsOnCleanupFailure(t *testing.T) {
	api := &fakeAPI{deleteErr: errors.New("network unavailable"), stableRevokeErr: errors.New("network unavailable"), revokeSessionErr: errors.New("network unavailable")}
	store := &memoryStore{putErrs: []error{errors.New("keyring unavailable"), nil}}
	service := testService(api, store)
	privateKey := make(ed25519.PrivateKey, ed25519.PrivateKeySize)
	_, err := service.finishLogin(context.Background(), client.Session{AccessToken: "access", RefreshToken: "refresh", DeviceID: "dev-1", ExpiresIn: 300}, privateKey)
	if err == nil || store.put != 2 || len(store.value) == 0 || api.deleted != 1 || api.stableRevokes != 1 {
		t.Fatalf("api=%#v store=%#v err=%v", api, store, err)
	}
}

func TestStableRevokeProofIsDeviceBoundAndDomainSeparated(t *testing.T) {
	privateKey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	credentials := Credentials{DeviceID: "dev-1", DevicePrivateKey: base64.RawURLEncoding.EncodeToString(privateKey)}
	request, err := stableRevokeRequest(credentials)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := base64.RawURLEncoding.DecodeString(request.Proof)
	if err != nil || !ed25519.Verify(privateKey.Public().(ed25519.PublicKey), []byte("blazn-session-revoke-v1\ndev-1"), proof) {
		t.Fatal("stable revoke proof did not verify")
	}
	if ed25519.Verify(privateKey.Public().(ed25519.PublicKey), []byte("blazn-refresh-v1\ndev-1"), proof) {
		t.Fatal("stable revoke proof was not domain separated")
	}
}

func TestBeginLoginRefusesExistingValidSessionBeforeAuthorization(t *testing.T) {
	api := &fakeAPI{}
	store := &memoryStore{value: storedCredentials(t, "2030-01-01T00:00:00Z")}
	service := testService(api, store)
	if _, _, _, err := service.BeginLogin(context.Background()); err == nil || len(api.authorizationRequest.DevicePublicKey) != 0 {
		t.Fatalf("authorization request=%#v err=%v", api.authorizationRequest, err)
	}
}

func TestBeginLoginCleansRevokedStoredSessionBeforeReplacement(t *testing.T) {
	api := &fakeAPI{
		authorization: client.DeviceAuthorization{DeviceCode: "device-secret", UserCode: "ABCD-EFGH", VerificationURI: "https://login.example/device", Challenge: "server-challenge", ExpiresIn: 600, Interval: 1},
		currentErr:    &client.APIError{StatusCode: http.StatusUnauthorized, Body: client.ErrorBody{Code: "session_revoked"}},
		refreshErr:    &client.APIError{StatusCode: http.StatusUnauthorized, Body: client.ErrorBody{Code: "session_revoked"}},
	}
	store := &memoryStore{value: storedCredentials(t, "2030-01-01T00:00:00Z")}
	service := testService(api, store)
	if _, _, _, err := service.BeginLogin(context.Background()); err != nil || api.stableRevokes != 1 || store.deleted != 1 || len(api.authorizationRequest.DevicePublicKey) == 0 {
		t.Fatalf("api=%#v store=%#v err=%v", api, store, err)
	}
	if service.pendingRelease != nil {
		service.pendingRelease()
		service.pendingRelease = nil
	}
}

func TestStatusRefreshesAfterAccessExpiry(t *testing.T) {
	api := &fakeAPI{
		session:     client.Session{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresIn: 300, DeviceID: "dev-1"},
		current:     client.CurrentUser{User: client.User{ID: "user-1"}, Device: client.Device{ID: "dev-1"}},
		currentErrs: []error{&client.APIError{StatusCode: http.StatusUnauthorized, Body: client.ErrorBody{Code: "access_expired"}}, nil},
	}
	store := &memoryStore{value: storedCredentials(t, "2030-01-01T00:00:00Z")}
	status, err := testService(api, store).Status(context.Background())
	if err != nil || !status.Authenticated || api.refreshes != 1 || api.currentCalls != 2 {
		t.Fatalf("status=%#v api=%#v err=%v", status, api, err)
	}
}

func TestExplicitRetryAfterIsNotCappedToFallbackMaximum(t *testing.T) {
	api := &fakeAPI{
		exchangeErrs: []error{
			&client.APIError{StatusCode: http.StatusTooManyRequests, RetryAfter: 120, Body: client.ErrorBody{Code: "slow_down"}},
			context.Canceled,
		},
	}
	service := testService(api, &memoryStore{})
	service.pendingPrivateKey = make([]byte, ed25519.PrivateKeySize)
	service.pendingChallenge = "challenge"
	var slept time.Duration
	service.sleep = func(_ context.Context, duration time.Duration) error { slept = duration; return nil }
	_, _ = service.CompleteLogin(context.Background(), "code", time.Second)
	if slept != 120*time.Second {
		t.Fatalf("slept=%v want=2m", slept)
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

func TestCanonicalAuthOriginNormalizesCaseAndDefaultPort(t *testing.T) {
	got, err := canonicalAuthOrigin("https://BLAZN.Example:443/base/path")
	if err != nil || got != "https://blazn.example" {
		t.Fatalf("origin=%q err=%v", got, err)
	}
}

func TestPollingRejectsMismatchedStatusAndCode(t *testing.T) {
	for _, apiErr := range []*client.APIError{
		{StatusCode: http.StatusInternalServerError, Body: client.ErrorBody{Code: "authorization_pending"}},
		{StatusCode: http.StatusPreconditionRequired, Body: client.ErrorBody{Code: "internal_error"}},
		{StatusCode: http.StatusTooManyRequests, Body: client.ErrorBody{Code: "authorization_pending"}},
	} {
		api := &fakeAPI{exchangeErrs: []error{apiErr}}
		service := testService(api, &memoryStore{})
		service.pendingPrivateKey = make([]byte, ed25519.PrivateKeySize)
		service.pendingChallenge = "challenge"
		if _, err := service.CompleteLogin(context.Background(), "code", time.Second); !errors.Is(err, apiErr) {
			t.Fatalf("error=%v want=%v", err, apiErr)
		}
	}
}

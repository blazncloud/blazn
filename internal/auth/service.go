package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/KingJammin/blazn/internal/client"
)

const defaultAPIURL = "https://blazn.benpelo.com"

const (
	maxDeviceAuthorizationLifetime = 15 * time.Minute
	maxAccessTokenLifetime         = 24 * time.Hour
)

var ErrOriginMismatch = errors.New("stored Blazn session API origin does not match")

type API interface {
	CreateDeviceAuthorization(context.Context, client.DeviceAuthorizationRequest) (client.DeviceAuthorization, error)
	ExchangeDeviceAuthorization(context.Context, client.DeviceSessionRequest) (client.Session, error)
	RefreshSession(context.Context, client.RefreshSessionRequest) (client.Session, error)
	DeleteCurrentSession(context.Context, string) error
	GetCurrentUser(context.Context, string) (client.CurrentUser, error)
	ListDevices(context.Context, string) (client.DeviceList, error)
	RevokeDevice(context.Context, string, string) error
}

type StableDeviceSessionRevoker interface {
	RevokeSession(context.Context, client.RevokeSessionRequest) error
}

type Credentials struct {
	APIOrigin        string    `json:"apiOrigin"`
	AccessToken      string    `json:"accessToken"`
	RefreshToken     string    `json:"refreshToken"`
	DeviceID         string    `json:"deviceId"`
	ExpiresAt        time.Time `json:"expiresAt"`
	DevicePrivateKey string    `json:"devicePrivateKey"`
}

type LoginStart struct {
	UserCode                string `json:"userCode"`
	VerificationURI         string `json:"verificationUri"`
	VerificationURIComplete string `json:"verificationUriComplete,omitempty"`
	ExpiresIn               int    `json:"expiresIn"`
}

type LoginResult struct {
	Status   string        `json:"status"`
	DeviceID string        `json:"deviceId"`
	User     client.User   `json:"user"`
	Device   client.Device `json:"device"`
}

type StatusResult struct {
	Authenticated bool           `json:"authenticated"`
	Store         string         `json:"store"`
	User          *client.User   `json:"user,omitempty"`
	Device        *client.Device `json:"device,omitempty"`
}

type LogoutResult struct {
	Status        string `json:"status"`
	RemoteRevoked bool   `json:"remoteRevoked"`
}

type Service struct {
	api               API
	store             CredentialStore
	origin            string
	locker            CredentialLocker
	now               func() time.Time
	sleep             func(context.Context, time.Duration) error
	pendingPrivateKey ed25519.PrivateKey
	pendingChallenge  string
	pendingLogin      string
}

func NewService(api API, store CredentialStore) *Service {
	return newService(api, store, defaultAPIURL, noopCredentialLocker{})
}

func newService(api API, store CredentialStore, origin string, locker CredentialLocker) *Service {
	return &Service{
		api:    api,
		store:  store,
		origin: origin,
		locker: locker,
		now:    time.Now,
		sleep: func(ctx context.Context, duration time.Duration) error {
			timer := time.NewTimer(duration)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
}

func NewDefaultService() (*Service, error) {
	apiURL := os.Getenv("BLAZN_API_URL")
	if apiURL == "" {
		apiURL = defaultAPIURL
	}
	origin, err := canonicalAuthOrigin(apiURL)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(request *http.Request, _ []*http.Request) error {
			redirectOrigin, err := canonicalAuthOrigin(request.URL.String())
			if err != nil || redirectOrigin != origin {
				return errors.New("refusing cross-origin authentication redirect")
			}
			return nil
		},
	}
	api, err := client.New(apiURL, httpClient)
	if err != nil {
		return nil, err
	}
	store, err := NewSystemStoreForOrigin(origin)
	if err != nil {
		return nil, err
	}
	locker, err := newCredentialLocker(origin)
	if err != nil {
		return nil, err
	}
	return newService(api, store, origin, locker), nil
}

func (s *Service) BeginLogin(ctx context.Context) (LoginStart, string, time.Duration, error) {
	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var loginNonce string
	if err := s.locker.WithLock(lockCtx, func() error {
		if err := s.ensureLoginAllowedLocked(ctx); err != nil {
			return err
		}
		var err error
		loginNonce, err = s.locker.ClaimLogin(s.now(), maxDeviceAuthorizationLifetime)
		return err
	}); err != nil {
		return LoginStart{}, "", 0, err
	}
	releaseFence := func(cause error) error {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if err := s.locker.WithLock(cleanupCtx, func() error { return s.locker.ReleaseLogin(loginNonce) }); err != nil {
			return fmt.Errorf("%v; release pending login fence: %w", cause, err)
		}
		return cause
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return LoginStart{}, "", 0, releaseFence(fmt.Errorf("generate device key: %w", err))
	}
	deviceName, err := os.Hostname()
	if err != nil || deviceName == "" {
		deviceName = "blazn-cli"
	}
	authorization, err := s.api.CreateDeviceAuthorization(ctx, client.DeviceAuthorizationRequest{
		DevicePublicKey: base64.RawURLEncoding.EncodeToString(publicKey),
		DeviceName:      deviceName,
		Platform:        runtime.GOOS + "/" + runtime.GOARCH,
	})
	if err != nil {
		return LoginStart{}, "", 0, releaseFence(err)
	}
	if authorization.DeviceCode == "" || authorization.UserCode == "" || authorization.VerificationURI == "" || authorization.Challenge == "" || authorization.ExpiresIn <= 0 || authorization.Interval <= 0 || authorization.ExpiresIn > int(maxDeviceAuthorizationLifetime/time.Second) {
		return LoginStart{}, "", 0, releaseFence(errors.New("API returned an incomplete device authorization"))
	}
	s.pendingPrivateKey = privateKey
	s.pendingChallenge = authorization.Challenge
	s.pendingLogin = loginNonce
	start := LoginStart{
		UserCode:                authorization.UserCode,
		VerificationURI:         authorization.VerificationURI,
		VerificationURIComplete: authorization.VerificationURIComplete,
		ExpiresIn:               authorization.ExpiresIn,
	}
	return start, authorization.DeviceCode, time.Duration(authorization.Interval) * time.Second, nil
}

func (s *Service) ensureLoginAllowedLocked(ctx context.Context) error {
	if credentials, err := s.credentialsLocked(ctx); err == nil {
		if _, currentErr := s.api.GetCurrentUser(ctx, credentials.AccessToken); currentErr == nil {
			return errors.New("this API origin already has a valid Blazn session; run 'blazn auth logout' before logging in again")
		} else if client.IsCode(currentErr, "session_revoked") {
			refreshed, refreshErr := s.refreshCredentialsLocked(ctx, credentials)
			if refreshErr == nil {
				if _, retryErr := s.api.GetCurrentUser(ctx, refreshed.AccessToken); retryErr == nil {
					return errors.New("this API origin already has a valid Blazn session; run 'blazn auth logout' before logging in again")
				} else {
					return fmt.Errorf("verify existing Blazn session before login: %w", retryErr)
				}
			} else if !errors.Is(refreshErr, ErrNotFound) {
				return fmt.Errorf("safely replace existing Blazn session before login: %w", refreshErr)
			}
		} else {
			return fmt.Errorf("verify existing Blazn session before login: %w", currentErr)
		}
	} else if !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("check existing Blazn session before login: %w", err)
	}
	return nil
}

func (s *Service) CompleteLogin(ctx context.Context, deviceCode string, interval time.Duration) (result LoginResult, resultErr error) {
	loginNonce := s.pendingLogin
	s.pendingLogin = ""
	if loginNonce != "" {
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.locker.WithLock(cleanupCtx, func() error { return s.locker.ReleaseLogin(loginNonce) }); err != nil {
				result = LoginResult{}
				if resultErr == nil {
					resultErr = fmt.Errorf("release pending login fence: %w", err)
				} else {
					resultErr = fmt.Errorf("%v; release pending login fence: %w", resultErr, err)
				}
			}
		}()
	}
	if len(s.pendingPrivateKey) != ed25519.PrivateKeySize || s.pendingChallenge == "" {
		return LoginResult{}, errors.New("device authorization state is missing; restart login")
	}
	proof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.pendingPrivateKey, []byte(deviceProofPayload(deviceCode, s.pendingChallenge))))
	defer func() {
		for i := range s.pendingPrivateKey {
			s.pendingPrivateKey[i] = 0
		}
		s.pendingPrivateKey = nil
		s.pendingChallenge = ""
	}()
	if interval < time.Second {
		interval = time.Second
	}
	for {
		session, err := s.api.ExchangeDeviceAuthorization(ctx, client.DeviceSessionRequest{DeviceCode: deviceCode, Proof: proof})
		if err == nil {
			var result LoginResult
			entered := false
			lockErr := s.locker.WithLock(ctx, func() error {
				entered = true
				if loginNonce != "" {
					if err := s.locker.VerifyLogin(loginNonce, s.now()); err != nil {
						return s.cleanupIssuedSession(ctx, session, s.pendingPrivateKey, false, fmt.Errorf("pending login fence was lost before finalization: %w", err))
					}
				}
				var finishErr error
				result, finishErr = s.finishLogin(ctx, session, s.pendingPrivateKey)
				return finishErr
			})
			if lockErr != nil && !entered {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				revokeErr := s.revokeIssuedSession(cleanupCtx, session, s.pendingPrivateKey)
				if revokeErr == nil {
					return LoginResult{}, fmt.Errorf("credential lock failed after session exchange: %w; issued session was revoked", lockErr)
				}
				retainErr := s.locker.WithLock(cleanupCtx, func() error { return s.save(session, s.pendingPrivateKey) })
				if retainErr != nil {
					return LoginResult{}, fmt.Errorf("credential lock failed after session exchange: %v; remote cleanup failed: %v; retaining credentials failed: %w", lockErr, revokeErr, retainErr)
				}
				return LoginResult{}, fmt.Errorf("credential lock failed after session exchange: %v; remote cleanup failed and credentials were retained: %w", lockErr, revokeErr)
			}
			return result, lockErr
		}
		var apiErr *client.APIError
		if !errors.As(err, &apiErr) {
			return LoginResult{}, err
		}
		pending := apiErr.StatusCode == http.StatusPreconditionRequired && apiErr.Body.Code == "authorization_pending"
		slowDown := apiErr.StatusCode == http.StatusTooManyRequests && apiErr.Body.Code == "slow_down"
		if !pending && !slowDown {
			return LoginResult{}, err
		}
		if slowDown {
			if apiErr.RetryAfter > 0 {
				interval = time.Duration(apiErr.RetryAfter) * time.Second
			} else {
				interval += 5 * time.Second
				if interval > 30*time.Second {
					interval = 30 * time.Second
				}
			}
		}
		if err := s.sleep(ctx, interval); err != nil {
			return LoginResult{}, err
		}
	}
}

func (s *Service) Status(ctx context.Context) (StatusResult, error) {
	var result StatusResult
	err := s.locker.WithLock(ctx, func() error {
		var current client.CurrentUser
		credentials, err := s.withAccessRetryLocked(ctx, func(credentials Credentials) error {
			var err error
			current, err = s.api.GetCurrentUser(ctx, credentials.AccessToken)
			return err
		})
		if errors.Is(err, ErrNotFound) {
			result = StatusResult{Authenticated: false, Store: s.store.Description()}
			return nil
		}
		if err != nil {
			return err
		}
		if err != nil {
			if client.IsCode(err, "session_revoked") {
				if revokeErr := s.revokeSessionStable(ctx, credentials); revokeErr != nil {
					return fmt.Errorf("session may be superseded; stable device revocation was not confirmed and local credentials were preserved: %w", revokeErr)
				}
				if deleteErr := s.store.Delete(); deleteErr != nil {
					return deleteErr
				}
				result = StatusResult{Authenticated: false, Store: s.store.Description()}
				return nil
			}
			if isDefinitiveCredentialError(err) {
				if deleteErr := s.store.Delete(); deleteErr != nil {
					return deleteErr
				}
				result = StatusResult{Authenticated: false, Store: s.store.Description()}
				return nil
			}
			return err
		}
		result = StatusResult{Authenticated: true, Store: s.store.Description(), User: &current.User, Device: &current.Device}
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		return StatusResult{Authenticated: false, Store: s.store.Description()}, nil
	}
	return result, err
}

func (s *Service) Logout(ctx context.Context) (LogoutResult, error) {
	result := LogoutResult{Status: "logout_failed", RemoteRevoked: false}
	err := s.locker.WithLock(ctx, func() error {
		credentials, err := s.credentialsLocked(ctx)
		if errors.Is(err, ErrNotFound) {
			if err := s.locker.CancelLogin(); err != nil {
				return fmt.Errorf("cancel pending login: %w", err)
			}
			result = LogoutResult{Status: "logged_out", RemoteRevoked: true}
			return nil
		}
		if err != nil {
			return fmt.Errorf("refresh remote session; local session preserved for retry: %w", err)
		}
		if _, supported := s.api.(StableDeviceSessionRevoker); supported {
			if stableErr := s.revokeSessionStable(ctx, credentials); stableErr != nil {
				return fmt.Errorf("stable device-bound session revocation failed; local session preserved: %w", stableErr)
			}
			// Stable device proof does not depend on the current refresh-token
			// generation, so response loss and rotation cannot orphan the session.
		} else {
			remoteErr := s.api.DeleteCurrentSession(ctx, credentials.AccessToken)
			if remoteErr != nil {
				return fmt.Errorf("proof-bound session revocation is unavailable and access-token revocation failed; local session preserved: %w", remoteErr)
			}
		}
		if err := s.store.Delete(); err != nil {
			return fmt.Errorf("remove local session: %w", err)
		}
		result = LogoutResult{Status: "logged_out", RemoteRevoked: true}
		return nil
	})
	return result, err
}

func (s *Service) Devices(ctx context.Context) ([]client.Device, error) {
	var items []client.Device
	err := s.locker.WithLock(ctx, func() error {
		_, err := s.withAccessRetryLocked(ctx, func(credentials Credentials) error {
			devices, err := s.api.ListDevices(ctx, credentials.AccessToken)
			items = devices.Items
			return err
		})
		return err
	})
	return items, err
}

func (s *Service) RevokeDevice(ctx context.Context, deviceID string) error {
	return s.locker.WithLock(ctx, func() error {
		credentials, err := s.withAccessRetryLocked(ctx, func(credentials Credentials) error {
			return s.api.RevokeDevice(ctx, credentials.AccessToken, deviceID)
		})
		if err != nil {
			return err
		}
		if deviceID == credentials.DeviceID {
			return s.store.Delete()
		}
		return nil
	})
}

func (s *Service) withAccessRetryLocked(ctx context.Context, action func(Credentials) error) (Credentials, error) {
	credentials, err := s.credentialsLocked(ctx)
	if err != nil {
		return Credentials{}, err
	}
	if err := action(credentials); err != nil {
		if !client.IsCode(err, "access_expired") {
			return credentials, err
		}
		credentials, err = s.refreshCredentialsLocked(ctx, credentials)
		if err != nil {
			return Credentials{}, err
		}
		if err := action(credentials); err != nil {
			return credentials, err
		}
	}
	return credentials, nil
}

func (s *Service) credentialsLocked(ctx context.Context) (Credentials, error) {
	credentials, err := s.load()
	if err != nil {
		return Credentials{}, err
	}
	if credentials.ExpiresAt.After(s.now().Add(30 * time.Second)) {
		return credentials, nil
	}
	return s.refreshCredentialsLocked(ctx, credentials)
}

func (s *Service) refreshCredentialsLocked(ctx context.Context, credentials Credentials) (Credentials, error) {
	request, err := refreshProofRequest(credentials)
	if err != nil {
		return Credentials{}, err
	}
	session, err := s.api.RefreshSession(ctx, request)
	if err != nil {
		if client.IsCode(err, "session_revoked") || client.IsCode(err, "refresh_invalid") {
			if revokeErr := s.revokeSessionStable(ctx, credentials); revokeErr != nil {
				return Credentials{}, fmt.Errorf("refresh credential may be stale; stable device revocation was not confirmed and local credentials were preserved: %v; refresh error: %w", revokeErr, err)
			}
			if deleteErr := s.store.Delete(); deleteErr != nil {
				return Credentials{}, deleteErr
			}
			return Credentials{}, ErrNotFound
		}
		if isDefinitiveCredentialError(err) {
			if deleteErr := s.store.Delete(); deleteErr != nil {
				return Credentials{}, deleteErr
			}
			return Credentials{}, ErrNotFound
		}
		return Credentials{}, err
	}
	privateKey, err := decodePrivateKey(credentials.DevicePrivateKey)
	if err != nil {
		return Credentials{}, err
	}
	if err := s.save(session, privateKey); err != nil {
		return Credentials{}, s.cleanupRotatedSession(ctx, session, privateKey, fmt.Errorf("store rotated session: %w", err))
	}
	return Credentials{APIOrigin: s.origin, AccessToken: session.AccessToken, RefreshToken: session.RefreshToken, DeviceID: session.DeviceID, ExpiresAt: s.now().Add(time.Duration(session.ExpiresIn) * time.Second), DevicePrivateKey: base64.RawURLEncoding.EncodeToString(privateKey)}, nil
}

func (s *Service) load() (Credentials, error) {
	encoded, err := s.store.Get()
	if err != nil {
		return Credentials{}, err
	}
	var credentials Credentials
	if err := json.Unmarshal(encoded, &credentials); err != nil {
		return Credentials{}, errors.New("stored Blazn session is invalid; run 'blazn auth logout'")
	}
	if credentials.APIOrigin == "" || credentials.AccessToken == "" || credentials.RefreshToken == "" || credentials.DeviceID == "" || credentials.ExpiresAt.IsZero() || credentials.DevicePrivateKey == "" {
		return Credentials{}, errors.New("stored Blazn session is incomplete; run 'blazn auth logout'")
	}
	if credentials.APIOrigin != s.origin {
		return Credentials{}, fmt.Errorf("%w: stored session belongs to %s, current API is %s", ErrOriginMismatch, credentials.APIOrigin, s.origin)
	}
	return credentials, nil
}

func (s *Service) save(session client.Session, privateKey ed25519.PrivateKey) error {
	if session.AccessToken == "" || session.RefreshToken == "" || session.DeviceID == "" || session.ExpiresIn <= 0 || session.ExpiresIn > int(maxAccessTokenLifetime/time.Second) {
		return errors.New("API returned an incomplete session")
	}
	if len(privateKey) != ed25519.PrivateKeySize {
		return errors.New("device private key is invalid")
	}
	credentials := Credentials{
		APIOrigin:        s.origin,
		AccessToken:      session.AccessToken,
		RefreshToken:     session.RefreshToken,
		DeviceID:         session.DeviceID,
		ExpiresAt:        s.now().Add(time.Duration(session.ExpiresIn) * time.Second),
		DevicePrivateKey: base64.RawURLEncoding.EncodeToString(privateKey),
	}
	encoded, err := json.Marshal(credentials)
	if err != nil {
		return err
	}
	return s.store.Put(encoded)
}

func (s *Service) finishLogin(ctx context.Context, session client.Session, privateKey ed25519.PrivateKey) (LoginResult, error) {
	if err := s.save(session, privateKey); err != nil {
		return LoginResult{}, s.cleanupIssuedSession(ctx, session, privateKey, false, fmt.Errorf("store session: %w", err))
	}
	current, err := s.api.GetCurrentUser(ctx, session.AccessToken)
	if err != nil {
		return LoginResult{}, s.cleanupIssuedSession(ctx, session, privateKey, true, fmt.Errorf("verify session: %w", err))
	}
	return LoginResult{Status: "authenticated", DeviceID: session.DeviceID, User: current.User, Device: current.Device}, nil
}

func (s *Service) cleanupIssuedSession(ctx context.Context, session client.Session, privateKey ed25519.PrivateKey, stored bool, cause error) error {
	revokeErr := s.revokeIssuedSession(ctx, session, privateKey)
	if revokeErr == nil {
		if stored {
			if err := s.store.Delete(); err != nil {
				return fmt.Errorf("%v; remote cleanup confirmed but local cleanup failed: %w", cause, err)
			}
		}
		return fmt.Errorf("%v; issued session was revoked", cause)
	}
	if !stored {
		if err := s.save(session, privateKey); err != nil {
			return fmt.Errorf("%v; remote cleanup failed: %v; retaining credentials also failed: %w", cause, revokeErr, err)
		}
	}
	return fmt.Errorf("%v; remote cleanup was not confirmed and credentials were retained for retry: %w", cause, revokeErr)
}

func (s *Service) cleanupRotatedSession(ctx context.Context, session client.Session, privateKey ed25519.PrivateKey, cause error) error {
	revokeErr := s.revokeIssuedSession(ctx, session, privateKey)
	if revokeErr == nil {
		if err := s.store.Delete(); err != nil {
			return fmt.Errorf("%v; rotated session was revoked but stale local credential cleanup failed: %w", cause, err)
		}
		return fmt.Errorf("%v; rotated session was revoked and stale local credentials were removed", cause)
	}
	if err := s.save(session, privateKey); err != nil {
		return fmt.Errorf("%v; remote cleanup failed: %v; retaining rotated credentials also failed: %w", cause, revokeErr, err)
	}
	return fmt.Errorf("%v; remote cleanup was not confirmed and rotated credentials were retained for retry: %w", cause, revokeErr)
}

func (s *Service) revokeIssuedSession(ctx context.Context, session client.Session, privateKey ed25519.PrivateKey) error {
	revokeErr := s.api.DeleteCurrentSession(ctx, session.AccessToken)
	if revokeErr != nil {
		credentials := Credentials{APIOrigin: s.origin, AccessToken: session.AccessToken, RefreshToken: session.RefreshToken, DeviceID: session.DeviceID, ExpiresAt: s.now(), DevicePrivateKey: base64.RawURLEncoding.EncodeToString(privateKey)}
		if stableErr := s.revokeSessionStable(ctx, credentials); stableErr == nil {
			revokeErr = nil
		}
	}
	return revokeErr
}

func (s *Service) revokeSessionStable(ctx context.Context, credentials Credentials) error {
	revoker, ok := s.api.(StableDeviceSessionRevoker)
	if !ok {
		return errors.New("stable device-bound session revocation is unavailable")
	}
	request, err := stableRevokeRequest(credentials)
	if err != nil {
		return err
	}
	if err := revoker.RevokeSession(ctx, request); err != nil && !isStableRevocationConfirmation(err) {
		return err
	}
	return nil
}

func stableRevokeRequest(credentials Credentials) (client.RevokeSessionRequest, error) {
	privateKey, err := decodePrivateKey(credentials.DevicePrivateKey)
	if err != nil {
		return client.RevokeSessionRequest{}, err
	}
	payload := "blazn-session-revoke-v1\n" + credentials.DeviceID
	proof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(payload)))
	return client.RevokeSessionRequest{DeviceID: credentials.DeviceID, Proof: proof}, nil
}

func isStableRevocationConfirmation(err error) bool {
	return client.IsCode(err, "device_revoked") || client.IsCode(err, "device_not_found") || client.IsCode(err, "session_invalid")
}

func refreshProofRequest(credentials Credentials) (client.RefreshSessionRequest, error) {
	privateKey, err := decodePrivateKey(credentials.DevicePrivateKey)
	if err != nil {
		return client.RefreshSessionRequest{}, err
	}
	digest := sha256.Sum256([]byte(credentials.RefreshToken))
	proofPayload := fmt.Sprintf("blazn-refresh-v1\n%s\n%x", credentials.DeviceID, digest)
	proof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(proofPayload)))
	return client.RefreshSessionRequest{RefreshToken: credentials.RefreshToken, DeviceID: credentials.DeviceID, Proof: proof}, nil
}

func deviceProofPayload(deviceCode, challenge string) string {
	return "blazn-device-session-v1\n" + deviceCode + "\n" + challenge
}

func decodePrivateKey(encoded string) (ed25519.PrivateKey, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) != ed25519.PrivateKeySize {
		return nil, errors.New("stored Blazn device key is invalid; run 'blazn auth logout'")
	}
	return ed25519.PrivateKey(decoded), nil
}

func isRefreshCredentialError(err error) bool {
	for _, code := range []string{"session_invalid", "refresh_invalid", "session_revoked", "device_revoked"} {
		if client.IsCode(err, code) {
			return true
		}
	}
	return false
}

func isDefinitiveCredentialError(err error) bool {
	for _, code := range []string{"session_invalid", "refresh_invalid", "session_revoked", "device_revoked"} {
		if client.IsCode(err, code) {
			return true
		}
	}
	return false
}

func OpenBrowser(uri string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name, args = "open", []string{uri}
	case "linux":
		name, args = "xdg-open", []string{uri}
	default:
		return fmt.Errorf("browser launch is unsupported on %s", runtime.GOOS)
	}
	if err := exec.Command(name, args...).Start(); err != nil {
		return fmt.Errorf("launch browser: %w", err)
	}
	return nil
}

func validateAuthAPIURL(value string) error {
	_, err := canonicalAuthOrigin(value)
	return err
}

func canonicalAuthOrigin(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("parse API URL: %w", err)
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("authentication API URL must contain only scheme, host, and optional base path")
	}
	scheme := strings.ToLower(parsed.Scheme)
	hostname := strings.ToLower(parsed.Hostname())
	if scheme != "https" && !(scheme == "http" && (hostname == "localhost" || hostname == "127.0.0.1" || hostname == "::1") && os.Getenv("BLAZN_ALLOW_INSECURE_LOCALHOST") == "1") {
		return "", errors.New("authentication API must use HTTPS; insecure HTTP is allowed only for an explicitly enabled loopback test server")
	}
	port := parsed.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	host := hostname
	if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	}
	return (&url.URL{Scheme: scheme, Host: host}).String(), nil
}

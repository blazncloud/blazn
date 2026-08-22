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
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/KingJammin/blazn/internal/client"
)

const defaultAPIURL = "https://blazn.benpelo.com"

const (
	maxDeviceAuthorizationLifetime = 15 * time.Minute
	maxAccessTokenLifetime         = 24 * time.Hour
)

type API interface {
	CreateDeviceAuthorization(context.Context, client.DeviceAuthorizationRequest) (client.DeviceAuthorization, error)
	ExchangeDeviceAuthorization(context.Context, client.DeviceSessionRequest) (client.Session, error)
	RefreshSession(context.Context, client.RefreshSessionRequest) (client.Session, error)
	DeleteCurrentSession(context.Context, string) error
	GetCurrentUser(context.Context, string) (client.CurrentUser, error)
	ListDevices(context.Context, string) (client.DeviceList, error)
	RevokeDevice(context.Context, string, string) error
}

type Credentials struct {
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
	now               func() time.Time
	sleep             func(context.Context, time.Duration) error
	pendingPrivateKey ed25519.PrivateKey
	pendingChallenge  string
}

func NewService(api API, store CredentialStore) *Service {
	return &Service{
		api:   api,
		store: store,
		now:   time.Now,
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
	if err := validateAuthAPIURL(apiURL); err != nil {
		return nil, err
	}
	api, err := client.New(apiURL, &http.Client{Timeout: 30 * time.Second})
	if err != nil {
		return nil, err
	}
	store, err := NewSystemStore()
	if err != nil {
		return nil, err
	}
	return NewService(api, store), nil
}

func (s *Service) BeginLogin(ctx context.Context) (LoginStart, string, time.Duration, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return LoginStart{}, "", 0, fmt.Errorf("generate device key: %w", err)
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
		return LoginStart{}, "", 0, err
	}
	if authorization.DeviceCode == "" || authorization.UserCode == "" || authorization.VerificationURI == "" || authorization.Challenge == "" || authorization.ExpiresIn <= 0 || authorization.Interval <= 0 || authorization.ExpiresIn > int(maxDeviceAuthorizationLifetime/time.Second) {
		return LoginStart{}, "", 0, errors.New("API returned an incomplete device authorization")
	}
	s.pendingPrivateKey = privateKey
	s.pendingChallenge = authorization.Challenge
	start := LoginStart{
		UserCode:                authorization.UserCode,
		VerificationURI:         authorization.VerificationURI,
		VerificationURIComplete: authorization.VerificationURIComplete,
		ExpiresIn:               authorization.ExpiresIn,
	}
	return start, authorization.DeviceCode, time.Duration(authorization.Interval) * time.Second, nil
}

func (s *Service) CompleteLogin(ctx context.Context, deviceCode string, interval time.Duration) (LoginResult, error) {
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
			if err := s.save(session, s.pendingPrivateKey); err != nil {
				return LoginResult{}, fmt.Errorf("store session: %w", err)
			}
			current, err := s.api.GetCurrentUser(ctx, session.AccessToken)
			if err != nil {
				_ = s.store.Delete()
				return LoginResult{}, fmt.Errorf("verify session: %w", err)
			}
			return LoginResult{Status: "authenticated", DeviceID: session.DeviceID, User: current.User, Device: current.Device}, nil
		}
		var apiErr *client.APIError
		if !errors.As(err, &apiErr) || (apiErr.StatusCode != http.StatusPreconditionRequired && apiErr.Body.Code != "authorization_pending" && apiErr.Body.Code != "slow_down") {
			return LoginResult{}, err
		}
		if apiErr.Body.Code == "slow_down" {
			interval += 5 * time.Second
		}
		if err := s.sleep(ctx, interval); err != nil {
			return LoginResult{}, err
		}
	}
}

func (s *Service) Status(ctx context.Context) (StatusResult, error) {
	credentials, err := s.credentials(ctx)
	if errors.Is(err, ErrNotFound) {
		return StatusResult{Authenticated: false, Store: s.store.Description()}, nil
	}
	if err != nil {
		return StatusResult{}, err
	}
	current, err := s.api.GetCurrentUser(ctx, credentials.AccessToken)
	if err != nil {
		if isTerminalSessionError(err) {
			_ = s.store.Delete()
			return StatusResult{Authenticated: false, Store: s.store.Description()}, nil
		}
		return StatusResult{}, err
	}
	return StatusResult{Authenticated: true, Store: s.store.Description(), User: &current.User, Device: &current.Device}, nil
}

func (s *Service) Logout(ctx context.Context) (LogoutResult, error) {
	credentials, err := s.credentials(ctx)
	if errors.Is(err, ErrNotFound) {
		return LogoutResult{Status: "logged_out", RemoteRevoked: true}, nil
	}
	if err != nil {
		return LogoutResult{Status: "logout_failed", RemoteRevoked: false}, fmt.Errorf("refresh remote session; local session preserved for retry: %w", err)
	}
	remoteErr := s.api.DeleteCurrentSession(ctx, credentials.AccessToken)
	if remoteErr != nil && !isTerminalSessionError(remoteErr) {
		return LogoutResult{Status: "logout_failed", RemoteRevoked: false}, fmt.Errorf("revoke remote session; local session preserved for retry: %w", remoteErr)
	}
	if err := s.store.Delete(); err != nil {
		return LogoutResult{}, fmt.Errorf("remove local session: %w", err)
	}
	return LogoutResult{Status: "logged_out", RemoteRevoked: true}, nil
}

func (s *Service) Devices(ctx context.Context) ([]client.Device, error) {
	credentials, err := s.credentials(ctx)
	if err != nil {
		return nil, err
	}
	devices, err := s.api.ListDevices(ctx, credentials.AccessToken)
	return devices.Items, err
}

func (s *Service) RevokeDevice(ctx context.Context, deviceID string) error {
	credentials, err := s.credentials(ctx)
	if err != nil {
		return err
	}
	if err := s.api.RevokeDevice(ctx, credentials.AccessToken, deviceID); err != nil {
		return err
	}
	if deviceID == credentials.DeviceID {
		return s.store.Delete()
	}
	return nil
}

func (s *Service) credentials(ctx context.Context) (Credentials, error) {
	credentials, err := s.load()
	if err != nil {
		return Credentials{}, err
	}
	if credentials.ExpiresAt.After(s.now().Add(30 * time.Second)) {
		return credentials, nil
	}
	privateKey, err := decodePrivateKey(credentials.DevicePrivateKey)
	if err != nil {
		return Credentials{}, err
	}
	digest := sha256.Sum256([]byte(credentials.RefreshToken))
	proofPayload := fmt.Sprintf("blazn-refresh-v1\n%s\n%x", credentials.DeviceID, digest)
	proof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(proofPayload)))
	session, err := s.api.RefreshSession(ctx, client.RefreshSessionRequest{RefreshToken: credentials.RefreshToken, DeviceID: credentials.DeviceID, Proof: proof})
	if err != nil {
		if isTerminalSessionError(err) {
			_ = s.store.Delete()
			return Credentials{}, ErrNotFound
		}
		return Credentials{}, err
	}
	if err := s.save(session, privateKey); err != nil {
		return Credentials{}, err
	}
	return Credentials{AccessToken: session.AccessToken, RefreshToken: session.RefreshToken, DeviceID: session.DeviceID, ExpiresAt: s.now().Add(time.Duration(session.ExpiresIn) * time.Second), DevicePrivateKey: base64.RawURLEncoding.EncodeToString(privateKey)}, nil
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
	if credentials.AccessToken == "" || credentials.RefreshToken == "" || credentials.DeviceID == "" || credentials.ExpiresAt.IsZero() || credentials.DevicePrivateKey == "" {
		return Credentials{}, errors.New("stored Blazn session is incomplete; run 'blazn auth logout'")
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

func isTerminalSessionError(err error) bool {
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
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("parse API URL: %w", err)
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1") && os.Getenv("BLAZN_ALLOW_INSECURE_LOCALHOST") == "1" {
		return nil
	}
	return errors.New("authentication API must use HTTPS; insecure HTTP is allowed only for an explicitly enabled loopback test server")
}

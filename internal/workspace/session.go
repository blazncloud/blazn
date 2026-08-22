package workspace

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/KingJammin/blazn/internal/auth"
	"github.com/KingJammin/blazn/internal/client"
)

const defaultAPIURL = "https://blazn.benpelo.com"

type Session struct {
	AccessToken string
	UserID      string
}

type SessionProvider interface {
	Session(context.Context, bool) (Session, error)
	Origin() string
}

type authSessionProvider struct {
	origin  string
	baseURL string
	api     *client.Client
	store   auth.CredentialStore
	lock    string
	now     func() time.Time
}

func NewDefaultSessionProvider() (SessionProvider, error) {
	apiURL := os.Getenv("BLAZN_API_URL")
	if apiURL == "" {
		apiURL = defaultAPIURL
	}
	origin, err := canonicalOrigin(apiURL)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	api, err := client.New(apiURL, httpClient)
	if err != nil {
		return nil, err
	}
	current, err := user.Current()
	if err != nil {
		return nil, err
	}
	accountDigest := sha256.Sum256([]byte(origin))
	account := "session-" + hex.EncodeToString(accountDigest[:16])
	lockDir := filepath.Join(current.HomeDir, ".local", "share", "blazn", "locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(lockDir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("Blazn credential lock directory is unsafe")
	} else if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Getuid()) {
		return nil, errors.New("Blazn credential lock directory has the wrong owner")
	}
	provider := &authSessionProvider{origin: origin, baseURL: apiURL, api: api, lock: filepath.Join(lockDir, account+".lock"), now: time.Now}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := provider.withLock(ctx, func() error {
		provider.store, err = auth.NewSystemStoreForOrigin(origin)
		return err
	}); err != nil {
		return nil, err
	}
	return provider, nil
}

func (p *authSessionProvider) Origin() string { return p.origin }

func (p *authSessionProvider) Session(ctx context.Context, forceRefresh bool) (Session, error) {
	var result Session
	err := p.withLock(ctx, func() error {
		credentials, err := p.load()
		if err != nil {
			return err
		}
		if forceRefresh || !credentials.ExpiresAt.After(p.now().Add(30*time.Second)) {
			credentials, err = p.refresh(ctx, credentials)
			if err != nil {
				return err
			}
		}
		current, err := p.api.GetCurrentUser(ctx, credentials.AccessToken)
		if client.IsCode(err, "access_expired") && !forceRefresh {
			credentials, err = p.refresh(ctx, credentials)
			if err == nil {
				current, err = p.api.GetCurrentUser(ctx, credentials.AccessToken)
			}
		}
		if err != nil {
			return err
		}
		result = Session{AccessToken: credentials.AccessToken, UserID: current.User.ID}
		return nil
	})
	return result, err
}

func (p *authSessionProvider) load() (auth.Credentials, error) {
	encoded, err := p.store.Get()
	if err != nil {
		return auth.Credentials{}, err
	}
	var credentials auth.Credentials
	if err := json.Unmarshal(encoded, &credentials); err != nil {
		return auth.Credentials{}, errors.New("stored Blazn session is invalid")
	}
	if credentials.APIOrigin != p.origin || credentials.AccessToken == "" || credentials.RefreshToken == "" || credentials.DeviceID == "" || credentials.DevicePrivateKey == "" {
		return auth.Credentials{}, auth.ErrOriginMismatch
	}
	return credentials, nil
}

func (p *authSessionProvider) refresh(ctx context.Context, credentials auth.Credentials) (auth.Credentials, error) {
	privateKey, err := base64.RawURLEncoding.DecodeString(credentials.DevicePrivateKey)
	if err != nil || len(privateKey) != ed25519.PrivateKeySize {
		return auth.Credentials{}, errors.New("stored Blazn device key is invalid")
	}
	digest := sha256.Sum256([]byte(credentials.RefreshToken))
	payload := fmt.Sprintf("blazn-refresh-v1\n%s\n%x", credentials.DeviceID, digest)
	proof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(ed25519.PrivateKey(privateKey), []byte(payload)))
	updated, err := p.api.RefreshSession(ctx, client.RefreshSessionRequest{RefreshToken: credentials.RefreshToken, DeviceID: credentials.DeviceID, Proof: proof})
	if err != nil {
		return auth.Credentials{}, err
	}
	if updated.AccessToken == "" || updated.RefreshToken == "" || updated.DeviceID == "" || updated.ExpiresIn < 1 {
		return auth.Credentials{}, errors.New("Blazn API returned an incomplete refreshed session")
	}
	credentials.AccessToken = updated.AccessToken
	credentials.RefreshToken = updated.RefreshToken
	credentials.DeviceID = updated.DeviceID
	credentials.ExpiresAt = p.now().Add(time.Duration(updated.ExpiresIn) * time.Second)
	encoded, err := json.Marshal(credentials)
	if err != nil {
		return auth.Credentials{}, err
	}
	if err := p.store.Put(encoded); err != nil {
		storeErr := err
		revokePayload := "blazn-session-revoke-v1\n" + credentials.DeviceID
		revokeProof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(ed25519.PrivateKey(privateKey), []byte(revokePayload)))
		revokeErr := p.api.RevokeSession(ctx, client.RevokeSessionRequest{DeviceID: credentials.DeviceID, Proof: revokeProof})
		if revokeErr == nil || client.IsCode(revokeErr, "device_not_found") || client.IsCode(revokeErr, "device_revoked") {
			if deleteErr := p.store.Delete(); deleteErr != nil {
				return auth.Credentials{}, fmt.Errorf("store refreshed session: %v; remote session revoked but stale credential cleanup failed: %w", storeErr, deleteErr)
			}
			return auth.Credentials{}, fmt.Errorf("store refreshed session: %w; remote session was revoked", storeErr)
		}
		if retryErr := p.store.Put(encoded); retryErr != nil {
			return auth.Credentials{}, fmt.Errorf("store refreshed session: %v; remote revocation failed: %v; credential retry failed: %w", storeErr, revokeErr, retryErr)
		}
	}
	return credentials, nil
}

func (p *authSessionProvider) withLock(ctx context.Context, action func() error) error {
	fd, err := syscall.Open(p.lock, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), p.lock)
	defer file.Close()
	if info, err := file.Stat(); err != nil {
		return err
	} else if err := validatePrivateFile(info); err != nil {
		return fmt.Errorf("credential lock is unsafe: %w", err)
	}
	for {
		if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			break
		} else if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	return action()
}

func canonicalOrigin(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("invalid Blazn API URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	hostname := strings.ToLower(parsed.Hostname())
	if scheme != "https" && !(scheme == "http" && (hostname == "localhost" || hostname == "127.0.0.1" || hostname == "::1") && os.Getenv("BLAZN_ALLOW_INSECURE_LOCALHOST") == "1") {
		return "", errors.New("Blazn API must use HTTPS")
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

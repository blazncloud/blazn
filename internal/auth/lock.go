package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"syscall"
	"time"
)

type CredentialLocker interface {
	Acquire(context.Context) (func(), error)
	WithLock(context.Context, func() error) error
	ClaimLogin(time.Time, time.Duration) (string, error)
	VerifyLogin(string, time.Time) error
	ReleaseLogin(string) error
	CancelLogin() error
}

var ErrLoginPending = errors.New("another Blazn login is pending for this API origin")

type noopCredentialLocker struct{}

func (noopCredentialLocker) Acquire(context.Context) (func(), error)               { return func() {}, nil }
func (noopCredentialLocker) WithLock(_ context.Context, action func() error) error { return action() }
func (noopCredentialLocker) ClaimLogin(time.Time, time.Duration) (string, error) {
	return "noop-login", nil
}
func (noopCredentialLocker) VerifyLogin(string, time.Time) error { return nil }
func (noopCredentialLocker) ReleaseLogin(string) error           { return nil }
func (noopCredentialLocker) CancelLogin() error                  { return nil }

type fileCredentialLocker struct{ path string }

type loginFenceRecord struct {
	Nonce     string    `json:"nonce"`
	PID       int       `json:"pid"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func newCredentialLocker(origin string) (CredentialLocker, error) {
	current, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("resolve current user for credential lock: %w", err)
	}
	return newCredentialLockerAtHome(origin, current.HomeDir)
}

func (l *fileCredentialLocker) pendingPath() string { return l.path + ".login" }

func (l *fileCredentialLocker) ClaimLogin(now time.Time, lifetime time.Duration) (string, error) {
	record, err := l.readLoginFence()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err == nil {
		if record.ExpiresAt.After(now) && processAlive(record.PID) {
			return "", ErrLoginPending
		}
		if err := l.removeLoginFence(); err != nil {
			return "", err
		}
	}
	bytes := make([]byte, 24)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate login fence nonce: %w", err)
	}
	nonce := base64.RawURLEncoding.EncodeToString(bytes)
	record = loginFenceRecord{Nonce: nonce, PID: os.Getpid(), ExpiresAt: now.Add(lifetime)}
	if err := l.writeLoginFence(record); err != nil {
		return "", err
	}
	return nonce, nil
}

func (l *fileCredentialLocker) VerifyLogin(nonce string, now time.Time) error {
	record, err := l.readLoginFence()
	if err != nil {
		return fmt.Errorf("read pending login fence: %w", err)
	}
	if record.Nonce != nonce || record.PID != os.Getpid() || !record.ExpiresAt.After(now) {
		return errors.New("pending login fence is no longer owned by this process")
	}
	return nil
}

func (l *fileCredentialLocker) ReleaseLogin(nonce string) error {
	record, err := l.readLoginFence()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if record.Nonce != nonce {
		return errors.New("refusing to release a pending login owned by another process")
	}
	return l.removeLoginFence()
}

func (l *fileCredentialLocker) CancelLogin() error {
	if _, err := l.readLoginFence(); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return l.removeLoginFence()
}

func (l *fileCredentialLocker) readLoginFence() (loginFenceRecord, error) {
	if err := validateOwnedMode(l.pendingPath(), false); err != nil {
		return loginFenceRecord{}, err
	}
	encoded, err := os.ReadFile(l.pendingPath())
	if err != nil {
		return loginFenceRecord{}, err
	}
	if len(encoded) > 4096 {
		return loginFenceRecord{}, errors.New("pending login fence is too large")
	}
	var record loginFenceRecord
	if err := json.Unmarshal(encoded, &record); err != nil || record.Nonce == "" || record.PID <= 0 || record.ExpiresAt.IsZero() {
		return loginFenceRecord{}, errors.New("pending login fence is invalid")
	}
	return record, nil
}

func (l *fileCredentialLocker) writeLoginFence(record loginFenceRecord) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	dir := filepath.Dir(l.path)
	temp, err := os.CreateTemp(dir, ".login-fence.tmp.*")
	if err != nil {
		return fmt.Errorf("create login fence: %w", err)
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temp.Write(encoded); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, l.pendingPath()); err != nil {
		return err
	}
	committed = true
	return syncDirectory(dir, "login fence creation")
}

func (l *fileCredentialLocker) removeLoginFence() error {
	if err := validateOwnedMode(l.pendingPath(), false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.Remove(l.pendingPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(l.path), "login fence removal")
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func newCredentialLockerAtHome(origin, home string) (CredentialLocker, error) {
	if !filepath.IsAbs(home) {
		return nil, errors.New("credential lock home must be absolute")
	}
	base := filepath.Join(home, ".local", "share")
	if !filepath.IsAbs(base) {
		return nil, errors.New("credential lock base must be absolute")
	}
	dir := filepath.Join(base, "blazn", "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create credential lock directory: %w", err)
	}
	if err := validateOwnedMode(dir, true); err != nil {
		return nil, fmt.Errorf("credential lock directory is unsafe: %w", err)
	}
	return &fileCredentialLocker{path: filepath.Join(dir, credentialAccountForOrigin(origin)+".lock")}, nil
}

func (l *fileCredentialLocker) WithLock(ctx context.Context, action func() error) error {
	release, err := l.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	return action()
}

func (l *fileCredentialLocker) Acquire(ctx context.Context) (func(), error) {
	fd, err := syscall.Open(l.path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open credential lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), l.path)
	if err := validateOwnedMode(l.path, false); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("credential lock is unsafe: %w", err)
	}
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			_ = file.Close()
			return nil, fmt.Errorf("acquire credential lock: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("acquire credential lock: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	return func() {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

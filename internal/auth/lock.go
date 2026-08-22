package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type CredentialLocker interface {
	WithLock(context.Context, func() error) error
}

type noopCredentialLocker struct{}

func (noopCredentialLocker) WithLock(_ context.Context, action func() error) error { return action() }

type fileCredentialLocker struct{ path string }

func newCredentialLocker(origin string) (CredentialLocker, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.Getenv("XDG_DATA_HOME")
	}
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locate credential lock directory: %w", err)
		}
		base = filepath.Join(home, ".local", "share")
	}
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
	fd, err := syscall.Open(l.path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("open credential lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), l.path)
	defer file.Close()
	if err := validateOwnedMode(l.path, false); err != nil {
		return fmt.Errorf("credential lock is unsafe: %w", err)
	}
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return fmt.Errorf("acquire credential lock: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("acquire credential lock: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	return action()
}

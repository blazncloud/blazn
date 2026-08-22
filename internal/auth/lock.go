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
	Acquire(context.Context) (func(), error)
	WithLock(context.Context, func() error) error
}

type noopCredentialLocker struct{}

func (noopCredentialLocker) Acquire(context.Context) (func(), error)               { return func() {}, nil }
func (noopCredentialLocker) WithLock(_ context.Context, action func() error) error { return action() }

type fileCredentialLocker struct{ path string }

func newCredentialLocker(origin string) (CredentialLocker, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate credential lock directory: %w", err)
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

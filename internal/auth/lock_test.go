package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCredentialLockSerializesSameOrigin(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	first, err := newCredentialLocker("https://example.test")
	if err != nil {
		t.Fatal(err)
	}
	second, err := newCredentialLocker("https://example.test")
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- first.WithLock(context.Background(), func() error {
			close(acquired)
			<-release
			return nil
		})
	}()
	<-acquired
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if err := second.WithLock(ctx, func() error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contending lock error = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCredentialLocksAreScopedByOrigin(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	first, _ := newCredentialLocker("https://one.example")
	second, _ := newCredentialLocker("https://two.example")
	if first.(*fileCredentialLocker).path == second.(*fileCredentialLocker).path {
		t.Fatal("different origins share one lock")
	}
}

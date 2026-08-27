package auth

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCredentialLockSerializesSameOrigin(t *testing.T) {
	home := t.TempDir()
	first, err := newCredentialLockerAtHome("https://example.test", home)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newCredentialLockerAtHome("https://example.test", home)
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

func TestCredentialLockSerializesAcrossProcesses(t *testing.T) {
	if os.Getenv("BLAZN_LOCK_HELPER") == "1" {
		locker, err := newCredentialLockerAtHome("https://example.test", os.Getenv("BLAZN_LOCK_TEST_HOME"))
		if err != nil {
			t.Fatal(err)
		}
		marker := os.Getenv("BLAZN_LOCK_MARKER")
		release := os.Getenv("BLAZN_LOCK_RELEASE")
		if err := locker.WithLock(context.Background(), func() error {
			if err := os.WriteFile(marker, []byte("locked"), 0o600); err != nil {
				return err
			}
			for {
				if _, err := os.Stat(release); err == nil {
					return nil
				}
				time.Sleep(10 * time.Millisecond)
			}
		}); err != nil {
			t.Fatal(err)
		}
		return
	}

	runtimeDir := t.TempDir()
	marker := filepath.Join(runtimeDir, "marker")
	release := filepath.Join(runtimeDir, "release")
	command := exec.Command(os.Args[0], "-test.run=^TestCredentialLockSerializesAcrossProcesses$")
	command.Env = append(os.Environ(), "BLAZN_LOCK_HELPER=1", "BLAZN_LOCK_TEST_HOME="+runtimeDir, "BLAZN_LOCK_MARKER="+marker, "BLAZN_LOCK_RELEASE="+release)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill() })
	// Package-wide CI can compile and start many subprocess helpers at once.
	// Keep the assertion bounded without treating a briefly saturated runner as
	// a credential-lock failure.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not acquire the credential lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	locker, err := newCredentialLockerAtHome("https://example.test", runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if err := locker.WithLock(ctx, func() error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cross-process contention error = %v", err)
	}
	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialLocksAreScopedByOrigin(t *testing.T) {
	home := t.TempDir()
	first, _ := newCredentialLockerAtHome("https://one.example", home)
	second, _ := newCredentialLockerAtHome("https://two.example", home)
	if first.(*fileCredentialLocker).path == second.(*fileCredentialLocker).path {
		t.Fatal("different origins share one lock")
	}
}

func TestCredentialLockPathIgnoresXDGEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "runtime-one"))
	first, err := newCredentialLockerAtHome("https://example.test", home)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "runtime-two"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data-two"))
	second, err := newCredentialLockerAtHome("https://example.test", home)
	if err != nil {
		t.Fatal(err)
	}
	if first.(*fileCredentialLocker).path != second.(*fileCredentialLocker).path {
		t.Fatalf("lock paths differ: %q %q", first.(*fileCredentialLocker).path, second.(*fileCredentialLocker).path)
	}
}

func TestPendingLoginFenceBlocksLiveOwnerAndReclaimsExpiry(t *testing.T) {
	home := t.TempDir()
	locker, err := newCredentialLockerAtHome("https://example.test", home)
	if err != nil {
		t.Fatal(err)
	}
	var first string
	if err := locker.WithLock(context.Background(), func() error {
		var err error
		first, err = locker.ClaimLogin(time.Now(), time.Minute)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := locker.WithLock(context.Background(), func() error {
		_, err := locker.ClaimLogin(time.Now(), time.Minute)
		return err
	}); !errors.Is(err, ErrLoginPending) {
		t.Fatalf("duplicate pending login error = %v", err)
	}
	if err := locker.WithLock(context.Background(), func() error { return locker.ReleaseLogin(first) }); err != nil {
		t.Fatal(err)
	}
	if err := locker.WithLock(context.Background(), func() error {
		_, err := locker.ClaimLogin(time.Now().Add(-time.Minute), time.Second)
		if err != nil {
			return err
		}
		_, err = locker.ClaimLogin(time.Now(), time.Minute)
		return err
	}); err != nil {
		t.Fatalf("expired pending login was not reclaimed: %v", err)
	}
	_ = locker.WithLock(context.Background(), locker.CancelLogin)
}

func TestPendingLoginFenceReclaimsDeadProcess(t *testing.T) {
	if os.Getenv("BLAZN_LOGIN_FENCE_HELPER") == "1" {
		locker, err := newCredentialLockerAtHome("https://example.test", os.Getenv("BLAZN_LOGIN_FENCE_HOME"))
		if err != nil {
			t.Fatal(err)
		}
		if err := locker.WithLock(context.Background(), func() error {
			_, err := locker.ClaimLogin(time.Now(), time.Minute)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	home := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestPendingLoginFenceReclaimsDeadProcess$")
	command.Env = append(os.Environ(), "BLAZN_LOGIN_FENCE_HELPER=1", "BLAZN_LOGIN_FENCE_HOME="+home)
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	locker, err := newCredentialLockerAtHome("https://example.test", home)
	if err != nil {
		t.Fatal(err)
	}
	if err := locker.WithLock(context.Background(), func() error {
		nonce, err := locker.ClaimLogin(time.Now(), time.Minute)
		if err != nil {
			return err
		}
		return locker.ReleaseLogin(nonce)
	}); err != nil {
		t.Fatalf("dead pending login was not reclaimed: %v", err)
	}
}

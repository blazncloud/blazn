package auth

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

type runnerCall struct {
	name  string
	args  []string
	stdin []byte
}

func TestProtectedFileStoreRecoversCredentialStagingAfterSIGKILL(t *testing.T) {
	if os.Getenv("BLAZN_STAGING_KILL_HELPER") == "1" {
		store, err := newProtectedFileStoreAtAccount(os.Getenv("BLAZN_STAGING_DIR"), "session.json")
		if err != nil {
			t.Fatal(err)
		}
		fileStore := store.(*protectedFileStore)
		staging, err := os.CreateTemp(fileStore.dir, fileStore.stagingPrefix()+"*")
		if err != nil {
			t.Fatal(err)
		}
		if err := staging.Chmod(0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := staging.Write([]byte("live-refresh-token-and-device-key")); err != nil {
			t.Fatal(err)
		}
		if err := staging.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv("BLAZN_STAGING_MARKER"), []byte(staging.Name()), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		return
	}

	dir := filepath.Join(t.TempDir(), "credentials")
	store, err := newProtectedFileStoreAtAccount(dir, "session.json")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "marker")
	command := exec.Command(os.Args[0], "-test.run=^TestProtectedFileStoreRecoversCredentialStagingAfterSIGKILL$")
	command.Env = append(os.Environ(), "BLAZN_STAGING_KILL_HELPER=1", "BLAZN_STAGING_DIR="+dir, "BLAZN_STAGING_MARKER="+marker)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	// Package-wide CI can compile and start many subprocess helpers at once.
	// Keep the assertion bounded without treating a briefly saturated runner as
	// a credential-recovery failure.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			t.Fatal("SIGKILL helper did not create a staging credential")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("SIGKILL helper exited successfully")
	}
	locker, err := newCredentialLockerAtHome("https://recovery.example", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var getErr error
	if err := locker.WithLock(context.Background(), func() error {
		_, getErr = store.Get()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(getErr, ErrNotFound) {
		t.Fatalf("Get after recovery = %v", getErr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), store.(*protectedFileStore).stagingPrefix()) {
			t.Fatalf("stale staging credential remains: %s", entry.Name())
		}
	}
}

type fakeRunner struct {
	paths map[string]bool
	out   []byte
	err   error
	calls []runnerCall
}

func (f *fakeRunner) LookPath(name string) (string, error) {
	if f.paths[name] {
		return "/usr/bin/" + name, nil
	}
	return "", errors.New("not found")
}

func (f *fakeRunner) Run(name string, args []string, stdin []byte) ([]byte, error) {
	f.calls = append(f.calls, runnerCall{name: name, args: append([]string(nil), args...), stdin: append([]byte(nil), stdin...)})
	return f.out, f.err
}

func (f *fakeRunner) RunPasswordPrompt(name string, args []string, _ string, secret []byte) error {
	f.calls = append(f.calls, runnerCall{name: name, args: append([]string(nil), args...), stdin: append([]byte(nil), secret...)})
	return f.err
}

func TestLinuxStoreUsesNamespacedSecretServiceEntry(t *testing.T) {
	runner := &fakeRunner{paths: map[string]bool{"secret-tool": true}}
	store, err := newSystemStoreForOriginAtHome("linux", runner, defaultAPIURL, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put([]byte(`{"refreshToken":"secret"}`)); err != nil {
		t.Fatal(err)
	}
	account := credentialAccountForOrigin(defaultAPIURL)
	wantArgs := []string{"store", "--label", "Blazn CLI session", "service", credentialService, "account", account}
	if got := runner.calls[2]; got.name != "secret-tool" || !reflect.DeepEqual(got.args, wantArgs) || string(got.stdin) != "{\"refreshToken\":\"secret\"}\n" {
		t.Fatalf("call = %#v", got)
	}
}

func TestDarwinStoreUsesNamespacedKeychainEntry(t *testing.T) {
	runner := &fakeRunner{paths: map[string]bool{"security": true}}
	t.Setenv(darwinCredentialBackendEnvName, "")
	store, err := newSystemStoreForOriginAtHome("darwin", runner, defaultAPIURL, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put([]byte("session")); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("Keychain calls = %#v", runner.calls)
	}
	want := []string{"add-generic-password", "-U", "-s", credentialService, "-a", credentialAccountForOrigin(defaultAPIURL), "-w"}
	if got := runner.calls[0]; got.name != "security" || !reflect.DeepEqual(got.args, want) || string(got.stdin) != "session" {
		t.Fatalf("call = %#v", got)
	}
}

func TestDarwinExplicitProtectedBackendPersistsInReceipt(t *testing.T) {
	home := t.TempDir()
	runner := &fakeRunner{paths: map[string]bool{"security": true}}
	t.Setenv(darwinCredentialBackendEnvName, backendProtectedFile)
	first, err := newSystemStoreForOriginAtHome("darwin", runner, "https://example.test", home)
	if err != nil || first.Description() != "protected credential file" {
		t.Fatalf("first=%T description=%q err=%v", first, first.Description(), err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("explicit protected backend touched Keychain: %#v", runner.calls)
	}
	if err := first.Put([]byte("headless-darwin-session")); err != nil {
		t.Fatal(err)
	}
	t.Setenv(darwinCredentialBackendEnvName, "")
	second, err := newSystemStoreForOriginAtHome("darwin", runner, "https://example.test", home)
	if err != nil || second.Description() != "protected credential file" {
		t.Fatalf("second=%T description=%q err=%v", second, second.Description(), err)
	}
	got, err := second.Get()
	if err != nil || string(got) != "headless-darwin-session" {
		t.Fatalf("Get=%q err=%v", got, err)
	}
	receipt := filepath.Join(home, ".local", "share", "blazn", "credentials", credentialAccountForOrigin("https://example.test")+".backend")
	info, err := os.Stat(receipt)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("receipt mode=%v err=%v", info.Mode(), err)
	}
}

func TestDarwinProtectedBackendReceiptRejectsConflictingOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv(darwinCredentialBackendEnvName, backendProtectedFile)
	if _, err := newSystemStoreForOriginAtHome("darwin", &fakeRunner{}, "https://example.test", home); err != nil {
		t.Fatal(err)
	}
	t.Setenv(darwinCredentialBackendEnvName, "keychain")
	if _, err := newSystemStoreForOriginAtHome("darwin", &fakeRunner{paths: map[string]bool{"security": true}}, "https://example.test", home); err == nil || !strings.Contains(err.Error(), "receipt selects") {
		t.Fatalf("conflicting override error=%v", err)
	}
}

func TestDarwinCredentialBackendRejectsUnsupportedOverride(t *testing.T) {
	t.Setenv(darwinCredentialBackendEnvName, "keychain")
	if _, err := newSystemStoreForOriginAtHome("darwin", &fakeRunner{paths: map[string]bool{"security": true}}, "https://example.test", t.TempDir()); err == nil || !strings.Contains(err.Error(), "must be") {
		t.Fatalf("unsupported override error=%v", err)
	}
}

func TestLinuxWithoutSecretServiceUsesProtectedStandaloneStore(t *testing.T) {
	store, err := newSystemStoreForOriginAtHome("linux", &fakeRunner{}, defaultAPIURL, t.TempDir())
	if err != nil || store.Description() != "protected credential file" {
		t.Fatalf("store=%T description=%q err=%v", store, store.Description(), err)
	}
}

func TestGetAndDeleteAreRedactionSafeAndIdempotent(t *testing.T) {
	runner := &fakeRunner{paths: map[string]bool{"secret-tool": true}, out: []byte("secret-session\n")}
	store, _ := newSystemStoreForOriginAtHome("linux", runner, defaultAPIURL, t.TempDir())
	runner.calls = nil
	got, err := store.Get()
	if err != nil || string(got) != "secret-session" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	runner.err = &commandError{message: "exit status 1", exitCode: 1}
	if err := store.Delete(); err != nil {
		t.Fatalf("Delete = %v", err)
	}
}

func TestSecretServiceBackendFailuresAreNotReportedAsMissingOrDeleted(t *testing.T) {
	runner := &fakeRunner{paths: map[string]bool{"secret-tool": true}}
	store, err := newSystemStoreForOriginAtHome("linux", runner, defaultAPIURL, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner.err = &commandError{message: "Cannot autolaunch D-Bus", exitCode: 1, stderr: true}
	if _, err := store.Get(); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("backend lookup error = %v", err)
	}
	if err := store.Delete(); err == nil {
		t.Fatal("backend clear failure was ignored")
	}
}

func TestLinuxBackendSelectionPersistsAcrossOutageRecovery(t *testing.T) {
	home := t.TempDir()
	outage := &fakeRunner{paths: map[string]bool{"secret-tool": true}, err: &commandError{message: "no session bus", exitCode: 1, stderr: true}}
	first, err := newSystemStoreForOriginAtHome("linux", outage, "https://example.test", home)
	if err != nil || first.Description() != "protected credential file" {
		t.Fatalf("first=%T description=%q err=%v", first, first.Description(), err)
	}
	healthy := &fakeRunner{paths: map[string]bool{"secret-tool": true}}
	second, err := newSystemStoreForOriginAtHome("linux", healthy, "https://example.test", home)
	if err != nil || second.Description() != "protected credential file" || len(healthy.calls) != 2 {
		t.Fatalf("second=%T description=%q calls=%#v err=%v", second, second.Description(), healthy.calls, err)
	}
}

func TestLinuxReceiptStillDetectsLaterBackendConflict(t *testing.T) {
	home := t.TempDir()
	outage := &fakeRunner{paths: map[string]bool{"secret-tool": true}, err: &commandError{message: "no session bus", exitCode: 1, stderr: true}}
	protected, err := newSystemStoreForOriginAtHome("linux", outage, "https://example.test", home)
	if err != nil {
		t.Fatal(err)
	}
	if err := protected.Put([]byte("protected-session")); err != nil {
		t.Fatal(err)
	}
	secret := &fakeRunner{paths: map[string]bool{"secret-tool": true}, out: []byte("secret-service-session\n")}
	if _, err := newSystemStoreForOriginAtHome("linux", secret, "https://example.test", home); err == nil || !strings.Contains(err.Error(), "also contains") {
		t.Fatalf("receipt conflict error=%v", err)
	}
}

func TestLinuxSelectedSecretServiceOutageFailsWithoutSwitching(t *testing.T) {
	home := t.TempDir()
	healthy := &fakeRunner{paths: map[string]bool{"secret-tool": true}}
	first, err := newSystemStoreForOriginAtHome("linux", healthy, "https://example.test", home)
	if err != nil || first.Description() != "Secret Service" {
		t.Fatalf("first=%T description=%q err=%v", first, first.Description(), err)
	}
	outage := &fakeRunner{paths: map[string]bool{"secret-tool": true}, err: &commandError{message: "no session bus", exitCode: 1, stderr: true}}
	if _, err := newSystemStoreForOriginAtHome("linux", outage, "https://example.test", home); err == nil || !strings.Contains(err.Error(), "refusing backend switch") {
		t.Fatalf("outage error=%v", err)
	}
}

func TestLinuxExistingFallbackCredentialWinsInitialSelection(t *testing.T) {
	home := t.TempDir()
	protected, err := newProtectedFileStoreForOriginAtHome("https://example.test", home)
	if err != nil {
		t.Fatal(err)
	}
	if err := protected.Put([]byte("protected-session")); err != nil {
		t.Fatal(err)
	}
	healthy := &fakeRunner{paths: map[string]bool{"secret-tool": true}}
	selected, err := newSystemStoreForOriginAtHome("linux", healthy, "https://example.test", home)
	if err != nil || selected.Description() != "protected credential file" {
		t.Fatalf("selected=%T description=%q err=%v", selected, selected.Description(), err)
	}
}

func TestLinuxConflictingCredentialBackendsFailClosed(t *testing.T) {
	home := t.TempDir()
	protected, err := newProtectedFileStoreForOriginAtHome("https://example.test", home)
	if err != nil {
		t.Fatal(err)
	}
	if err := protected.Put([]byte("protected-session")); err != nil {
		t.Fatal(err)
	}
	secret := &fakeRunner{paths: map[string]bool{"secret-tool": true}, out: []byte("secret-service-session\n")}
	if _, err := newSystemStoreForOriginAtHome("linux", secret, "https://example.test", home); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("conflict error=%v", err)
	}
}

func TestSecretServiceProbeFallsBackToProtectedStore(t *testing.T) {
	runner := &fakeRunner{paths: map[string]bool{"secret-tool": true}, err: &commandError{message: "no session bus", exitCode: 1, stderr: true}}
	store, err := newSystemStoreForOriginAtHome("linux", runner, "https://example.test", t.TempDir())
	if err != nil || store.Description() != "protected credential file" {
		t.Fatalf("store=%T description=%q err=%v", store, store.Description(), err)
	}
}

func TestCredentialStoresAreNamespacedByCanonicalOrigin(t *testing.T) {
	home := t.TempDir()
	first, err := newProtectedFileStoreForOriginAtHome("https://one.example", home)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newProtectedFileStoreForOriginAtHome("https://two.example", home)
	if err != nil {
		t.Fatal(err)
	}
	if first.(*protectedFileStore).path == second.(*protectedFileStore).path {
		t.Fatal("different origins share a credential path")
	}
}

func TestProtectedCredentialPathIgnoresHomeAndXDGEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	first, err := newProtectedFileStoreForOriginAtHome("https://example.test", home)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	second, err := newProtectedFileStoreForOriginAtHome("https://example.test", home)
	if err != nil {
		t.Fatal(err)
	}
	if first.(*protectedFileStore).path != second.(*protectedFileStore).path {
		t.Fatalf("credential paths differ: %q %q", first.(*protectedFileStore).path, second.(*protectedFileStore).path)
	}
}

func TestProtectedFileStoreRoundTripAndRejectsUnsafeFiles(t *testing.T) {
	store, err := newProtectedFileStoreAt(filepath.Join(t.TempDir(), "credentials"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put([]byte("session-secret")); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get()
	if err != nil || string(got) != "session-secret" {
		t.Fatalf("Get=%q err=%v", got, err)
	}
	fileStore := store.(*protectedFileStore)
	info, err := os.Stat(fileStore.path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v", info.Mode(), err)
	}
	if err := store.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete = %v", err)
	}

	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("do-not-read"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, fileStore.path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("unsafe symlink error = %v", err)
	}
}

func TestProtectedFileDeleteSyncsParentDirectory(t *testing.T) {
	store, err := newProtectedFileStoreAt(filepath.Join(t.TempDir(), "credentials"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put([]byte("session-secret")); err != nil {
		t.Fatal(err)
	}
	fileStore := store.(*protectedFileStore)
	var operation string
	fileStore.syncHook = func(dir, gotOperation string) error {
		if dir != fileStore.dir {
			t.Fatalf("sync dir=%q want=%q", dir, fileStore.dir)
		}
		operation = gotOperation
		return nil
	}
	if err := store.Delete(); err != nil {
		t.Fatal(err)
	}
	if operation != "credential deletion" {
		t.Fatalf("sync operation=%q", operation)
	}
}

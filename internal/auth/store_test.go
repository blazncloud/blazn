package auth

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type runnerCall struct {
	name  string
	args  []string
	stdin []byte
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

func (f *fakeRunner) RunPasswordPrompt(name string, args []string, secret []byte) error {
	f.calls = append(f.calls, runnerCall{name: name, args: append([]string(nil), args...), stdin: append([]byte(nil), secret...)})
	return f.err
}

func TestLinuxStoreUsesNamespacedSecretServiceEntry(t *testing.T) {
	runner := &fakeRunner{paths: map[string]bool{"secret-tool": true}}
	store, err := newSystemStore("linux", runner)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put([]byte(`{"refreshToken":"secret"}`)); err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"store", "--label", "Blazn CLI session", "service", credentialService, "account", credentialAccount}
	if got := runner.calls[0]; got.name != "secret-tool" || !reflect.DeepEqual(got.args, wantArgs) || string(got.stdin) != "{\"refreshToken\":\"secret\"}\n" {
		t.Fatalf("call = %#v", got)
	}
}

func TestDarwinStoreUsesNamespacedKeychainEntry(t *testing.T) {
	runner := &fakeRunner{paths: map[string]bool{"security": true}}
	store, err := newSystemStore("darwin", runner)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put([]byte("session")); err != nil {
		t.Fatal(err)
	}
	want := []string{"add-generic-password", "-U", "-s", credentialService, "-a", credentialAccount, "-w"}
	if got := runner.calls[0]; got.name != "security" || !reflect.DeepEqual(got.args, want) || string(got.stdin) != "session" {
		t.Fatalf("call = %#v", got)
	}
}

func TestDarwinTestKeychainRequiresExplicitSafePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.keychain-db")
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BLAZN_TEST_KEYCHAIN_PATH", path)
	if _, err := selectedDarwinKeychainPath(); err == nil {
		t.Fatal("test keychain path worked without explicit opt-in")
	}
	t.Setenv("BLAZN_ALLOW_TEST_KEYCHAIN", "1")
	got, err := selectedDarwinKeychainPath()
	if err != nil || got != path {
		t.Fatalf("path=%q err=%v", got, err)
	}
}

func TestLinuxWithoutSecretServiceUsesProtectedStandaloneStore(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store, err := newSystemStore("linux", &fakeRunner{})
	if err != nil || store.Description() != "protected credential file" {
		t.Fatalf("store=%T description=%q err=%v", store, store.Description(), err)
	}
}

func TestGetAndDeleteAreRedactionSafeAndIdempotent(t *testing.T) {
	runner := &fakeRunner{paths: map[string]bool{"secret-tool": true}, out: []byte("secret-session\n")}
	store, _ := newSystemStore("linux", runner)
	got, err := store.Get()
	if err != nil || string(got) != "secret-session" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	runner.err = errors.New("not found")
	if err := store.Delete(); err != nil {
		t.Fatalf("Delete = %v", err)
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

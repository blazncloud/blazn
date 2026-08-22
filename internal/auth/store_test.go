package auth

import (
	"errors"
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
	if got := runner.calls[0]; got.name != "security" || !reflect.DeepEqual(got.args, want) || string(got.stdin) != "session\n" {
		t.Fatalf("call = %#v", got)
	}
}

func TestMissingSecureStoreFailsClosed(t *testing.T) {
	if _, err := newSystemStore("linux", &fakeRunner{}); err == nil {
		t.Fatal("expected missing Secret Service client to fail")
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

package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContextStoreIsOriginAndUserScopedAtomicAndPrivate(t *testing.T) {
	store, err := NewFileContextStoreAtHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	selection := Selection{APIOrigin: "https://one.example", UserID: "user-one", WorkspaceID: "workspace-one", SelectedAt: time.Now().UTC()}
	if err := store.Save(selection); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(selection.APIOrigin, selection.UserID)
	if err != nil || got.WorkspaceID != "workspace-one" {
		t.Fatalf("got=%#v err=%v", got, err)
	}
	info, err := os.Stat(store.path(selection.APIOrigin, selection.UserID))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v", info.Mode(), err)
	}
	if _, err := store.Load("https://two.example", selection.UserID); !errors.Is(err, ErrNoContext) {
		t.Fatalf("cross-origin err=%v", err)
	}
	if _, err := store.Load(selection.APIOrigin, "user-two"); !errors.Is(err, ErrNoContext) {
		t.Fatalf("cross-user err=%v", err)
	}
}

func TestContextStoreRejectsSymlink(t *testing.T) {
	home := t.TempDir()
	store, _ := NewFileContextStoreAtHome(home)
	origin, userID := "https://one.example", "user-one"
	path := store.path(origin, userID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte(`{"workspaceId":"stolen"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(origin, userID); err == nil {
		t.Fatal("symlink context accepted")
	}
}

func TestDefaultContextPathIgnoresAmbientHome(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store, err := NewFileContextStore()
	if err != nil {
		t.Fatal(err)
	}
	if store.home == fakeHome || strings.HasPrefix(store.path("https://one.example", "user"), fakeHome) {
		t.Fatalf("context trusted ambient home: %q", store.home)
	}
}

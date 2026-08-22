package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

func candidate(t *testing.T, directory, body string) string {
	t.Helper()
	path := filepath.Join(directory, "candidate-"+body)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStoreActivateValidateRollbackAndRemove(t *testing.T) {
	store, err := NewStoreAt(filepath.Join(t.TempDir(), "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := DefaultCatalog().Plugin("social")
	first := validManifest("v1.0.0")
	if _, err := store.Activate(definition, first, candidate(t, t.TempDir(), "one")); err != nil {
		t.Fatal(err)
	}
	installed, err := store.Current("social")
	if err != nil || installed.Receipt.Version != "v1.0.0" {
		t.Fatalf("first install = %#v, %v", installed, err)
	}
	second := validManifest("v1.1.0")
	if _, err := store.Activate(definition, second, candidate(t, t.TempDir(), "two")); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := store.Rollback("social")
	if err != nil || rolledBack.Version != "v1.0.0" || rolledBack.PreviousVersion != "v1.1.0" {
		t.Fatalf("rollback = %#v, %v", rolledBack, err)
	}
	if err := os.WriteFile(filepath.Join(store.root, "social", "versions", "v1.0.0", "blazn-social"), []byte("tampered"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Current("social"); err == nil {
		t.Fatal("tampered binary accepted")
	}
	// Restore through the retained alternate version before exercising owned removal.
	if _, err := store.Rollback("social"); err == nil {
		t.Fatal("rollback unexpectedly accepted a tampered current install")
	}
	if err := os.RemoveAll(filepath.Join(store.root, "social")); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRejectsPublicOrSymlinkDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	store, _ := NewStoreAt(root)
	definition, _ := DefaultCatalog().Plugin("social")
	if _, err := store.Activate(definition, validManifest("v1.0.0"), candidate(t, t.TempDir(), "one")); err == nil {
		t.Fatal("public plugin root accepted")
	}

	root = filepath.Join(t.TempDir(), "plugins")
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, root); err != nil {
		t.Fatal(err)
	}
	store, _ = NewStoreAt(root)
	if _, err := store.Activate(definition, validManifest("v1.0.0"), candidate(t, t.TempDir(), "two")); err == nil {
		t.Fatal("symlink plugin root accepted")
	}
}

func TestStoreRejectsConcurrentPluginMutation(t *testing.T) {
	store, err := NewStoreAt(filepath.Join(t.TempDir(), "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ensureRoot(); err != nil {
		t.Fatal(err)
	}
	release, err := store.acquireLock("social")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	definition, _ := DefaultCatalog().Plugin("social")
	_, err = store.Activate(definition, validManifest("v1.0.0"), candidate(t, t.TempDir(), "one"))
	if err == nil || err.Error() != "plugin operation is already in progress" {
		t.Fatalf("concurrent operation err=%v", err)
	}
}

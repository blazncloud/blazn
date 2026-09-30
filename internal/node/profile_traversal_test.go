package node

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSymlinkTraversalStillRejectsLinksAndUnreadableUnsafeParents(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := verifyNoSymlinkTraversal(filepath.Join(link, "child")); err == nil {
		t.Fatal("symbolic link traversal accepted")
	}
	if os.Geteuid() == 0 {
		t.Skip("permission-denied branch requires an unprivileged test user")
	}
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0700); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(locked, "inner")
	if err := os.Mkdir(inner, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0700)
	// The unreadable directory's parent is owned by the test user, not root,
	// so a permission error below it must still fail closed.
	if err := verifyNoSymlinkTraversal(filepath.Join(inner, "file")); err == nil {
		t.Fatal("permission error below a non-root-owned directory was accepted")
	}
	if rootOnlyDirectory(root) {
		t.Fatal("user-owned temporary directory treated as root-only")
	}
	if info, err := os.Lstat("/root"); err == nil && info.Mode().Perm()&0o022 == 0 && !rootOnlyDirectory("/") {
		t.Fatal("root-owned non-writable directory not recognized")
	}
}

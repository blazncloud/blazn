package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCopySecretProducesStrictReaderShape(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source")
	destinationDirectory := filepath.Join(directory, "private")
	if err := os.WriteFile(source, []byte("postgres://controller:secret@10.0.0.1:5432/blazn\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destinationDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(destinationDirectory, "database-url")
	if err := copySecret(source, destination); err != nil {
		t.Fatalf("copy secret: %v", err)
	}
	contents, err := os.ReadFile(destination)
	if err != nil || string(contents) != "postgres://controller:secret@10.0.0.1:5432/blazn" {
		t.Fatalf("unexpected private secret")
	}
	info, err := os.Lstat(destination)
	if err != nil || info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("unexpected private secret mode: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() || stat.Nlink != 1 {
		t.Fatal("private secret does not match strict reader ownership")
	}
}

func TestCopySecretFailsClosed(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source")
	private := filepath.Join(directory, "private")
	if err := os.WriteFile(source, []byte("postgres://controller@10.0.0.1/blazn"), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		prepare func() (string, string)
	}{
		{name: "relative source", prepare: func() (string, string) { return "source", filepath.Join(private, "relative") }},
		{name: "same path", prepare: func() (string, string) { return source, source }},
		{name: "existing destination", prepare: func() (string, string) {
			destination := filepath.Join(private, "existing")
			if err := os.WriteFile(destination, []byte("do-not-overwrite"), 0o600); err != nil {
				t.Fatal(err)
			}
			return source, destination
		}},
		{name: "oversized source", prepare: func() (string, string) {
			oversized := filepath.Join(directory, "oversized")
			if err := os.WriteFile(oversized, []byte(strings.Repeat("x", maxSecretBytes+1)), 0o440); err != nil {
				t.Fatal(err)
			}
			return oversized, filepath.Join(private, "oversized")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			src, destination := test.prepare()
			if err := copySecret(src, destination); err == nil {
				t.Fatal("unsafe copy succeeded")
			}
		})
	}
	contents, err := os.ReadFile(filepath.Join(private, "existing"))
	if err != nil || string(contents) != "do-not-overwrite" {
		t.Fatal("existing destination changed")
	}
}

func TestRunRequiresExactArguments(t *testing.T) {
	if run(nil) == nil || run([]string{"one"}) == nil || run([]string{"one", "two", "three"}) == nil {
		t.Fatal("invalid argument count accepted")
	}
}

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

func TestCopyCAProducesExactStrictReaderShape(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source-ca")
	destinationDirectory := filepath.Join(directory, "private")
	contents := []byte("\n-----BEGIN CERTIFICATE-----\nexact-ca-contents\n-----END CERTIFICATE-----\n")
	if err := os.WriteFile(source, contents, 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destinationDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(destinationDirectory, "kubernetes-ca.crt")
	if err := copyExactFile(source, destination, maxCABytes); err != nil {
		t.Fatalf("copy CA: %v", err)
	}
	copied, err := os.ReadFile(destination)
	if err != nil || string(copied) != string(contents) {
		t.Fatalf("CA contents changed during copy")
	}
	info, err := os.Lstat(destination)
	if err != nil || info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("unexpected private CA mode: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() || stat.Nlink != 1 {
		t.Fatal("private CA does not match strict reader ownership")
	}

	oversized := filepath.Join(directory, "oversized-ca")
	if err := os.WriteFile(oversized, []byte(strings.Repeat("x", maxCABytes+1)), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := copyExactFile(oversized, filepath.Join(destinationDirectory, "oversized-output"), maxCABytes); err == nil {
		t.Fatal("oversized CA was accepted")
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
	if run(nil) == nil || run([]string{"one", "two"}) == nil || run([]string{"one", "two", "three", "four"}) == nil ||
		run([]string{"one", "two", "three", "four", "five", "six", "seven", "two"}) == nil {
		t.Fatal("invalid argument count accepted")
	}
}

func TestRunCopiesBothPrivateFilesAndCleansPartialFailure(t *testing.T) {
	directory := t.TempDir()
	private := filepath.Join(directory, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	databaseSource := filepath.Join(directory, "database-url")
	caSource := filepath.Join(directory, "ca.crt")
	accessSource, objectSecretSource := filepath.Join(directory, "object-access"), filepath.Join(directory, "object-secret")
	if err := os.WriteFile(databaseSource, []byte(" postgres://controller@10.0.0.1/blazn\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	caContents := []byte("-----BEGIN CERTIFICATE-----\nexact\n-----END CERTIFICATE-----\n")
	if err := os.WriteFile(caSource, caContents, 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(accessSource, []byte("ACCESSKEY123456\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objectSecretSource, []byte("object-secret-material-123456\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	databaseDestination := filepath.Join(private, "database-url")
	caDestination := filepath.Join(private, "kubernetes-ca.crt")
	accessDestination, objectSecretDestination := filepath.Join(private, "object-access"), filepath.Join(private, "object-secret")
	if err := run([]string{databaseSource, databaseDestination, caSource, caDestination,
		accessSource, accessDestination, objectSecretSource, objectSecretDestination}); err != nil {
		t.Fatalf("initialize private files: %v", err)
	}
	if value, err := os.ReadFile(databaseDestination); err != nil || string(value) != "postgres://controller@10.0.0.1/blazn" {
		t.Fatal("database URL was not normalized into the private file")
	}
	if value, err := os.ReadFile(caDestination); err != nil || string(value) != string(caContents) {
		t.Fatal("CA was not copied exactly into the private file")
	}
	if value, err := os.ReadFile(accessDestination); err != nil || string(value) != "ACCESSKEY123456" {
		t.Fatal("object access key was not normalized into the private file")
	}
	if value, err := os.ReadFile(objectSecretDestination); err != nil || string(value) != "object-secret-material-123456" {
		t.Fatal("object secret was not normalized into the private file")
	}

	secondPrivate := filepath.Join(directory, "private-failure")
	if err := os.Mkdir(secondPrivate, 0o700); err != nil {
		t.Fatal(err)
	}
	partialDatabase := filepath.Join(secondPrivate, "database-url")
	if err := run([]string{databaseSource, partialDatabase, filepath.Join(directory, "missing-ca"), filepath.Join(secondPrivate, "kubernetes-ca.crt"),
		accessSource, filepath.Join(secondPrivate, "object-access"), objectSecretSource, filepath.Join(secondPrivate, "object-secret")}); err == nil {
		t.Fatal("missing CA source was accepted")
	}
	if _, err := os.Lstat(partialDatabase); !os.IsNotExist(err) {
		t.Fatal("partial database URL remained after CA copy failure")
	}
}

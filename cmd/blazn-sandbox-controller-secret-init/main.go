package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	maxSecretBytes = 16 * 1024
	maxCABytes     = 1 << 20
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox controller private file initialization failed")
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 4 || args[1] == args[3] {
		return errors.New("database URL and CA source and destination paths are required")
	}
	if err := copySecret(args[0], args[1]); err != nil {
		return err
	}
	if err := copyExactFile(args[2], args[3], maxCABytes); err != nil {
		_ = os.Remove(args[1])
		return err
	}
	return nil
}

func copySecret(source, destination string) error {
	return copyPrivateFile(source, destination, maxSecretBytes, func(contents []byte) ([]byte, bool) {
		value := strings.TrimSpace(string(contents))
		return []byte(value), value != "" && !strings.ContainsRune(value, '\x00')
	})
}

func copyExactFile(source, destination string, limit int64) error {
	return copyPrivateFile(source, destination, limit, func(contents []byte) ([]byte, bool) {
		return contents, len(contents) != 0
	})
}

func copyPrivateFile(source, destination string, limit int64, normalize func([]byte) ([]byte, bool)) error {
	if !validAbsolutePath(source) || !validAbsolutePath(destination) || source == destination {
		return errors.New("private file paths are invalid")
	}
	if limit < 1 || normalize == nil {
		return errors.New("private file policy is invalid")
	}
	directory := filepath.Dir(destination)
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("destination directory is unsafe")
	}

	input, err := os.Open(source)
	if err != nil {
		return errors.New("private file source cannot be opened")
	}
	defer input.Close()
	opened, err := input.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() < 1 || opened.Size() > limit {
		return errors.New("private file source is invalid")
	}
	contents, err := io.ReadAll(io.LimitReader(input, limit+1))
	if err != nil || len(contents) < 1 || int64(len(contents)) > limit {
		return errors.New("private file source cannot be read")
	}
	value, valid := normalize(contents)
	if !valid || len(value) < 1 || int64(len(value)) > limit {
		return errors.New("private file source is invalid")
	}

	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return errors.New("private secret destination cannot be created")
	}
	remove := true
	defer func() {
		_ = output.Close()
		if remove {
			_ = os.Remove(destination)
		}
	}()
	if _, err := output.Write(value); err != nil {
		return errors.New("private secret destination cannot be written")
	}
	if err := output.Sync(); err != nil {
		return errors.New("private secret destination cannot be synchronized")
	}
	if err := output.Close(); err != nil {
		return errors.New("private secret destination cannot be closed")
	}
	created, err := os.Lstat(destination)
	if err != nil || !created.Mode().IsRegular() || created.Mode().Perm() != 0o600 || created.Size() != int64(len(value)) {
		return errors.New("private secret destination is unsafe")
	}
	stat, ok := created.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() || stat.Nlink != 1 {
		return errors.New("private secret destination ownership is unsafe")
	}
	remove = false
	return nil
}

func validAbsolutePath(value string) bool {
	return filepath.IsAbs(value) && filepath.Clean(value) == value && !strings.ContainsRune(value, '\x00')
}

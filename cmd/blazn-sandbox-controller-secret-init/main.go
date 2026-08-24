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

const maxSecretBytes = 16 * 1024

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox controller database URL initialization failed")
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 2 {
		return errors.New("source and destination paths are required")
	}
	return copySecret(args[0], args[1])
}

func copySecret(source, destination string) error {
	if !validAbsolutePath(source) || !validAbsolutePath(destination) || source == destination {
		return errors.New("secret paths are invalid")
	}
	directory := filepath.Dir(destination)
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("destination directory is unsafe")
	}

	input, err := os.Open(source)
	if err != nil {
		return errors.New("secret source cannot be opened")
	}
	defer input.Close()
	opened, err := input.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() < 1 || opened.Size() > maxSecretBytes {
		return errors.New("secret source is invalid")
	}
	contents, err := io.ReadAll(io.LimitReader(input, maxSecretBytes+1))
	if err != nil || len(contents) < 1 || len(contents) > maxSecretBytes {
		return errors.New("secret source cannot be read")
	}
	value := strings.TrimSpace(string(contents))
	if value == "" || strings.ContainsRune(value, '\x00') {
		return errors.New("secret source is invalid")
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
	if _, err := output.Write([]byte(value)); err != nil {
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

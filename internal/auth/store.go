package auth

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

const (
	credentialService = "com.blazn.cli.v1alpha1"
	credentialAccount = "default-session"
)

var ErrNotFound = errors.New("Blazn session not found")

type CredentialStore interface {
	Get() ([]byte, error)
	Put([]byte) error
	Delete() error
	Description() string
}

type commandRunner interface {
	LookPath(string) (string, error)
	Run(name string, args []string, stdin []byte) ([]byte, error)
	RunPasswordPrompt(name string, args []string, secret []byte) error
}

type execRunner struct{}

func (execRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }

func (execRunner) Run(name string, args []string, stdin []byte) ([]byte, error) {
	command := exec.Command(name, args...)
	command.Stdin = bytes.NewReader(stdin)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, errors.New(message)
	}
	return output, nil
}

func (execRunner) RunPasswordPrompt(name string, args []string, secret []byte) error {
	return runPasswordPrompt(name, args, secret)
}

type systemStore struct {
	goos   string
	runner commandRunner
}

func NewSystemStore() (CredentialStore, error) {
	return newSystemStore(runtime.GOOS, execRunner{})
}

func newSystemStore(goos string, runner commandRunner) (CredentialStore, error) {
	store := &systemStore{goos: goos, runner: runner}
	var command string
	switch goos {
	case "darwin":
		command = "security"
	case "linux":
		command = "secret-tool"
	default:
		return nil, fmt.Errorf("secure credential storage is unsupported on %s", goos)
	}
	if _, err := runner.LookPath(command); err != nil {
		if goos == "linux" {
			return newProtectedFileStore()
		}
		return nil, fmt.Errorf("secure credential store %q is unavailable: %w", command, err)
	}
	return store, nil
}

type protectedFileStore struct {
	dir  string
	path string
}

func newProtectedFileStore() (CredentialStore, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locate credential data directory: %w", err)
		}
		base = filepath.Join(home, ".local", "share")
	}
	return newProtectedFileStoreAt(filepath.Join(base, "blazn", "credentials"))
}

func newProtectedFileStoreAt(dir string) (CredentialStore, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("credential directory must be absolute")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create credential directory: %w", err)
	}
	if err := validateOwnedMode(dir, true); err != nil {
		return nil, fmt.Errorf("credential directory is unsafe: %w", err)
	}
	return &protectedFileStore{dir: dir, path: filepath.Join(dir, "session.v1")}, nil
}

func (s *protectedFileStore) Description() string { return "protected credential file" }

func (s *protectedFileStore) Get() ([]byte, error) {
	if err := validateOwnedMode(s.path, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	fd, err := syscall.Open(s.path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open credential: %w", err)
	}
	file := os.NewFile(uintptr(fd), s.path)
	defer file.Close()
	limited := io.LimitReader(file, 1<<20)
	value, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read credential: %w", err)
	}
	if len(value) == 0 {
		return nil, ErrNotFound
	}
	return value, nil
}

func (s *protectedFileStore) Put(secret []byte) error {
	if len(secret) == 0 || len(secret) > 1<<20 {
		return errors.New("credential has an invalid size")
	}
	if err := validateOwnedMode(s.dir, true); err != nil {
		return fmt.Errorf("credential directory is unsafe: %w", err)
	}
	temp, err := os.CreateTemp(s.dir, ".session.tmp.*")
	if err != nil {
		return fmt.Errorf("create credential staging file: %w", err)
	}
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		_ = os.Remove(temp.Name())
		return fmt.Errorf("protect credential staging file: %w", err)
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err := temp.Write(secret); err != nil {
		return fmt.Errorf("write credential: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync credential: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close credential: %w", err)
	}
	if err := os.Rename(tempPath, s.path); err != nil {
		return fmt.Errorf("commit credential: %w", err)
	}
	committed = true
	directory, err := os.Open(s.dir)
	if err != nil {
		return fmt.Errorf("open credential directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync credential directory: %w", err)
	}
	return nil
}

func (s *protectedFileStore) Delete() error {
	if err := validateOwnedMode(s.path, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove credential: %w", err)
	}
	return nil
}

func validateOwnedMode(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if directory {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("expected a real directory")
		}
	} else if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("expected a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("permissions %04o allow group or other access", info.Mode().Perm())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return errors.New("owner does not match the current user")
	}
	if !directory && stat.Nlink != 1 {
		return errors.New("credential file has unexpected hard links")
	}
	return nil
}

func (s *systemStore) Description() string {
	if s.goos == "darwin" {
		return "macOS Keychain"
	}
	return "Secret Service"
}

func (s *systemStore) Get() ([]byte, error) {
	var output []byte
	var err error
	if s.goos == "darwin" {
		output, err = s.runner.Run("security", []string{"find-generic-password", "-s", credentialService, "-a", credentialAccount, "-w"}, nil)
	} else {
		output, err = s.runner.Run("secret-tool", []string{"lookup", "service", credentialService, "account", credentialAccount}, nil)
	}
	if err != nil || len(bytes.TrimSpace(output)) == 0 {
		return nil, ErrNotFound
	}
	return bytes.TrimSpace(output), nil
}

func (s *systemStore) Put(secret []byte) error {
	if len(secret) == 0 {
		return errors.New("refusing to store an empty session")
	}
	if s.goos == "darwin" {
		// A trailing -w asks on the controlling terminal. The runner provides a
		// private PTY so the value never appears in argv, environment, or logs.
		return s.runner.RunPasswordPrompt("security", []string{"add-generic-password", "-U", "-s", credentialService, "-a", credentialAccount, "-w"}, secret)
	}
	_, err := s.runner.Run("secret-tool", []string{"store", "--label", "Blazn CLI session", "service", credentialService, "account", credentialAccount}, append(secret, '\n'))
	return err
}

func (s *systemStore) Delete() error {
	var err error
	if s.goos == "darwin" {
		_, err = s.runner.Run("security", []string{"delete-generic-password", "-s", credentialService, "-a", credentialAccount}, nil)
	} else {
		_, err = s.runner.Run("secret-tool", []string{"clear", "service", credentialService, "account", credentialAccount}, nil)
	}
	if err != nil {
		// Deletion is idempotent: missing entries are already logged out.
		return nil
	}
	return nil
}

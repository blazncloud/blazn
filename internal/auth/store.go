package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

const (
	credentialService = "com.blazn.cli.v1alpha1"
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
	RunPasswordPrompt(name string, args []string, account string, secret []byte) error
}

type commandError struct {
	message  string
	exitCode int
	stderr   bool
}

func (e *commandError) Error() string { return e.message }

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
		hasStderr := message != ""
		if message == "" {
			message = err.Error()
		}
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		return nil, &commandError{message: message, exitCode: exitCode, stderr: hasStderr}
	}
	return output, nil
}

func (execRunner) RunPasswordPrompt(name string, args []string, account string, secret []byte) error {
	return storeDarwinCredential(account, secret)
}

type systemStore struct {
	goos    string
	runner  commandRunner
	account string
}

const (
	backendSecretService = "secret-service"
	backendProtectedFile = "protected-file"
)

func NewSystemStore() (CredentialStore, error) {
	return NewSystemStoreForOrigin(defaultAPIURL)
}

func newSystemStore(goos string, runner commandRunner) (CredentialStore, error) {
	return newSystemStoreForOrigin(goos, runner, defaultAPIURL)
}

func NewSystemStoreForOrigin(origin string) (CredentialStore, error) {
	return newSystemStoreForOrigin(runtime.GOOS, execRunner{}, origin)
}

func newSystemStoreForOrigin(goos string, runner commandRunner, origin string) (CredentialStore, error) {
	return newSystemStoreWithFallback(goos, runner, origin, func() (CredentialStore, error) {
		return newProtectedFileStoreForOrigin(origin)
	})
}

func newSystemStoreForOriginAtHome(goos string, runner commandRunner, origin, home string) (CredentialStore, error) {
	return newSystemStoreWithFallback(goos, runner, origin, func() (CredentialStore, error) {
		return newProtectedFileStoreForOriginAtHome(origin, home)
	})
}

func newSystemStoreWithFallback(goos string, runner commandRunner, origin string, fallback func() (CredentialStore, error)) (CredentialStore, error) {
	account := credentialAccountForOrigin(origin)
	store := &systemStore{goos: goos, runner: runner, account: account}
	switch goos {
	case "darwin":
		if _, err := runner.LookPath("security"); err != nil {
			return nil, fmt.Errorf("secure credential store %q is unavailable: %w", "security", err)
		}
		return store, nil
	case "linux":
		return selectLinuxCredentialBackend(runner, store, fallback, account)
	default:
		return nil, fmt.Errorf("secure credential storage is unsupported on %s", goos)
	}
}

func selectLinuxCredentialBackend(runner commandRunner, secretStore *systemStore, fallback func() (CredentialStore, error), account string) (CredentialStore, error) {
	protected, err := fallback()
	if err != nil {
		return nil, err
	}
	fileStore, ok := protected.(*protectedFileStore)
	if !ok {
		return nil, errors.New("protected credential fallback has an unexpected implementation")
	}
	receiptPath := filepath.Join(fileStore.dir, account+".backend")
	selected, err := readBackendReceipt(receiptPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		switch selected {
		case backendProtectedFile:
			if probeSecretService(runner) == nil {
				secretExists, err := credentialExists(secretStore)
				if err != nil {
					return nil, err
				}
				if secretExists {
					return nil, errors.New("credential backend receipt selects protected-file but Secret Service also contains credentials")
				}
			}
			return protected, nil
		case backendSecretService:
			if err := probeSecretService(runner); err != nil {
				return nil, fmt.Errorf("selected Secret Service backend is unavailable; refusing backend switch: %w", err)
			}
			protectedExists, err := credentialExists(protected)
			if err != nil {
				return nil, err
			}
			if protectedExists {
				return nil, errors.New("credential backend receipt selects Secret Service but protected-file also contains credentials")
			}
			return secretStore, nil
		default:
			return nil, fmt.Errorf("credential backend receipt has unsupported value %q", selected)
		}
	}

	protectedExists, err := credentialExists(protected)
	if err != nil {
		return nil, err
	}
	secretAvailable := probeSecretService(runner) == nil
	secretExists := false
	if secretAvailable {
		secretExists, err = credentialExists(secretStore)
		if err != nil {
			return nil, err
		}
	}
	if protectedExists && secretExists {
		return nil, errors.New("credentials exist in both Secret Service and protected-file backends; refusing ambiguous selection")
	}
	selection := backendProtectedFile
	chosen := protected
	if secretExists || (!protectedExists && secretAvailable) {
		selection = backendSecretService
		chosen = secretStore
	}
	if err := writeBackendReceipt(receiptPath, selection); err != nil {
		return nil, err
	}
	return chosen, nil
}

func probeSecretService(runner commandRunner) error {
	if _, err := runner.LookPath("secret-tool"); err != nil {
		return err
	}
	if _, err := runner.Run("secret-tool", []string{"search", "--all", "service", credentialService}, nil); err != nil && !isMissingSecretToolItem(err) {
		return err
	}
	return nil
}

func credentialExists(store CredentialStore) (bool, error) {
	_, err := store.Get()
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func readBackendReceipt(path string) (string, error) {
	if err := validateOwnedMode(path, false); err != nil {
		return "", err
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(encoded) > 64 {
		return "", errors.New("credential backend receipt is too large")
	}
	return strings.TrimSpace(string(encoded)), nil
}

func writeBackendReceipt(path, backend string) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".backend.tmp.*")
	if err != nil {
		return fmt.Errorf("create credential backend receipt: %w", err)
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temp.Write([]byte(backend + "\n")); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	committed = true
	return syncDirectory(dir, "credential backend selection")
}

func credentialAccountForOrigin(origin string) string {
	digest := sha256.Sum256([]byte(origin))
	return "session-" + hex.EncodeToString(digest[:16])
}

type protectedFileStore struct {
	dir      string
	path     string
	syncHook func(string, string) error
}

func newProtectedFileStore() (CredentialStore, error) {
	return newProtectedFileStoreForOrigin(defaultAPIURL)
}

func newProtectedFileStoreForOrigin(origin string) (CredentialStore, error) {
	current, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("resolve current user for credential storage: %w", err)
	}
	return newProtectedFileStoreForOriginAtHome(origin, current.HomeDir)

}

func newProtectedFileStoreForOriginAtHome(origin, home string) (CredentialStore, error) {
	if !filepath.IsAbs(home) {
		return nil, errors.New("credential storage home must be absolute")
	}
	base := filepath.Join(home, ".local", "share")
	return newProtectedFileStoreAtAccount(filepath.Join(base, "blazn", "credentials"), credentialAccountForOrigin(origin)+".json")
}

func newProtectedFileStoreAt(dir string) (CredentialStore, error) {
	return newProtectedFileStoreAtAccount(dir, "session.v1")
}

func newProtectedFileStoreAtAccount(dir, name string) (CredentialStore, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("credential directory must be absolute")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create credential directory: %w", err)
	}
	if err := validateOwnedMode(dir, true); err != nil {
		return nil, fmt.Errorf("credential directory is unsafe: %w", err)
	}
	if filepath.Base(name) != name || name == "." || name == "" {
		return nil, errors.New("credential filename is unsafe")
	}
	return &protectedFileStore{dir: dir, path: filepath.Join(dir, name)}, nil
}

func (s *protectedFileStore) Description() string { return "protected credential file" }

func (s *protectedFileStore) Get() ([]byte, error) {
	if err := s.reconcileStagingFiles(); err != nil {
		return nil, err
	}
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
	if err := s.reconcileStagingFiles(); err != nil {
		return err
	}
	temp, err := os.CreateTemp(s.dir, s.stagingPrefix()+"*")
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
	if err := s.reconcileStagingFiles(); err != nil {
		return err
	}
	if err := validateOwnedMode(s.path, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove credential: %w", err)
	}
	return s.syncDirectory("credential deletion")
}

func (s *protectedFileStore) stagingPrefix() string {
	return "." + filepath.Base(s.path) + ".tmp."
}

func (s *protectedFileStore) reconcileStagingFiles() error {
	if err := validateOwnedMode(s.dir, true); err != nil {
		return fmt.Errorf("credential directory is unsafe: %w", err)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("list credential staging files: %w", err)
	}
	removed := false
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), s.stagingPrefix()) {
			continue
		}
		path := filepath.Join(s.dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect credential staging file: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
			return fmt.Errorf("refusing unsafe credential staging file %s", path)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
			return fmt.Errorf("refusing unowned or linked credential staging file %s", path)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove stale credential staging file: %w", err)
		}
		removed = true
	}
	if !removed {
		return nil
	}
	return s.syncDirectory("credential staging reconciliation")
}

func (s *protectedFileStore) syncDirectory(operation string) error {
	if s.syncHook != nil {
		return s.syncHook(s.dir, operation)
	}
	return syncDirectory(s.dir, operation)
}

func syncDirectory(dir, operation string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open credential directory after %s: %w", operation, err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync credential directory after %s: %w", operation, err)
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
		return loadDarwinCredential(s.account)
	} else {
		output, err = s.runner.Run("secret-tool", []string{"lookup", "service", credentialService, "account", s.account}, nil)
	}
	if err != nil {
		if isMissingSecretToolItem(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read Secret Service credential: %w", err)
	}
	if len(bytes.TrimSpace(output)) == 0 {
		return nil, ErrNotFound
	}
	return bytes.TrimSpace(output), nil
}

func (s *systemStore) Put(secret []byte) error {
	if len(secret) == 0 {
		return errors.New("refusing to store an empty session")
	}
	if s.goos == "darwin" {
		// The runner uses Security.framework directly, so the value never appears
		// in argv, environment, stdin, or logs.
		return s.runner.RunPasswordPrompt("security", []string{"add-generic-password", "-U", "-s", credentialService, "-a", s.account, "-w"}, s.account, secret)
	}
	_, err := s.runner.Run("secret-tool", []string{"store", "--label", "Blazn CLI session", "service", credentialService, "account", s.account}, append(secret, '\n'))
	return err
}

func (s *systemStore) Delete() error {
	var err error
	if s.goos == "darwin" {
		return deleteDarwinCredential(s.account)
	} else {
		_, err = s.runner.Run("secret-tool", []string{"clear", "service", credentialService, "account", s.account}, nil)
	}
	if err != nil {
		if isMissingSecretToolItem(err) {
			return nil
		}
		return fmt.Errorf("delete Secret Service credential: %w", err)
	}
	return nil
}

func isMissingSecretToolItem(err error) bool {
	var commandErr *commandError
	return errors.As(err, &commandErr) && commandErr.exitCode == 1 && !commandErr.stderr
}

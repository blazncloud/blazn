package auth

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
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
		return nil, fmt.Errorf("secure credential store %q is unavailable: %w", command, err)
	}
	return store, nil
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
		// A trailing -w prompts on stdin. Supplying its value as an argument is
		// explicitly unsafe because other processes can inspect argv.
		_, err := s.runner.Run("security", []string{"add-generic-password", "-U", "-s", credentialService, "-a", credentialAccount, "-w"}, append(secret, '\n'))
		return err
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

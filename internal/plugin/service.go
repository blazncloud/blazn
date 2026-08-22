package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

type Status struct {
	Name       string   `json:"name"`
	Repository string   `json:"repository"`
	Commands   []string `json:"commands"`
	Installed  bool     `json:"installed"`
	Version    string   `json:"version,omitempty"`
	Healthy    bool     `json:"healthy"`
	Message    string   `json:"message,omitempty"`
}

type Stdio struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type ProcessRunner interface {
	Run(context.Context, string, []string, RuntimeContext, Stdio) (int, error)
}

type execProcessRunner struct{}

func (execProcessRunner) Run(ctx context.Context, path string, args []string, runtimeContext RuntimeContext, streams Stdio) (int, error) {
	command := exec.CommandContext(ctx, path, args...)
	command.Env = pluginEnvironment(os.Environ())
	command.Stdin, command.Stdout, command.Stderr = streams.Stdin, streams.Stdout, streams.Stderr
	environment, err := runtimeEnvironment(os.Environ(), runtimeContext)
	if err != nil {
		return 1, err
	}
	command.Env = environment
	err = command.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 1, err
}

func pluginEnvironment(environment []string) []string {
	blocked := map[string]bool{
		"BLAZN_PLUGIN_VERSION":    true,
		"GH_ENTERPRISE_TOKEN":     true,
		"GH_TOKEN":                true,
		"GITHUB_ENTERPRISE_TOKEN": true,
		"GITHUB_TOKEN":            true,
	}
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		key := entry
		if separator := strings.IndexByte(entry, '='); separator >= 0 {
			key = entry[:separator]
		}
		if !blocked[key] {
			result = append(result, entry)
		}
	}
	return result
}

type Service struct {
	Catalog     Catalog
	Store       *Store
	Installer   Installer
	Runner      ProcessRunner
	CoreVersion string
}

func NewService(coreVersion string) (*Service, error) {
	store, err := NewStore()
	if err != nil {
		return nil, err
	}
	return &Service{Catalog: DefaultCatalog(), Store: store, Installer: NewGitHubInstaller(), Runner: execProcessRunner{}, CoreVersion: coreVersion}, nil
}

func (s *Service) Resolve(command string) (Definition, bool) { return s.Catalog.Resolve(command) }

func (s *Service) Installed(name string) (Installed, error) { return s.Store.Current(name) }

func (s *Service) Install(ctx context.Context, name string) (Receipt, error) {
	definition, ok := s.Catalog.Plugin(name)
	if !ok {
		return Receipt{}, fmt.Errorf("unknown plugin %q", name)
	}
	return s.Installer.Install(ctx, definition, s.CoreVersion, s.Store)
}

func (s *Service) List() []Status {
	result := make([]Status, 0)
	for _, definition := range s.Catalog.Plugins() {
		commands := append([]string{definition.CanonicalCommand}, definition.Aliases...)
		status := Status{Name: definition.Name, Repository: definition.Repository, Commands: commands}
		installed, err := s.Store.Current(definition.Name)
		if err == nil {
			status.Installed, status.Version = true, installed.Receipt.Version
			if compatibilityErr := Compatible(s.CoreVersion, installed.Receipt.Manifest); compatibilityErr != nil {
				status.Message = compatibilityErr.Error()
			} else {
				status.Healthy = true
			}
		} else if errors.Is(err, os.ErrNotExist) {
			status.Message = "not installed"
		} else {
			status.Installed, status.Message = true, err.Error()
		}
		result = append(result, status)
	}
	return result
}

func (s *Service) Rollback(name string) (Receipt, error) { return s.Store.Rollback(name) }
func (s *Service) Remove(name string) error              { return s.Store.Remove(name) }

func (s *Service) Run(ctx context.Context, definition Definition, args []string, format string, runtimeContext RuntimeContext, streams Stdio) (int, error) {
	if err := runtimeContext.Validate(); err != nil {
		return 0, fmt.Errorf("validate plugin runtime context: %w", err)
	}
	installed, err := s.Store.Current(definition.Name)
	if err != nil {
		return 0, err
	}
	if err := Compatible(s.CoreVersion, installed.Receipt.Manifest); err != nil {
		return 0, err
	}
	forwarded := append([]string(nil), args...)
	if format != "human" {
		forwarded = append(forwarded, "--output="+format)
	}
	if runtimeContext.OutputFormat != format {
		return 0, errors.New("plugin runtime context output format does not match dispatch")
	}
	return s.Runner.Run(ctx, installed.Path, forwarded, runtimeContext, streams)
}

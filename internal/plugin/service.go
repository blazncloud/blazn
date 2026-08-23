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
	Run(context.Context, string, []string, string, RuntimeContext, Stdio) (int, error)
}

type execProcessRunner struct{}

func (execProcessRunner) Run(ctx context.Context, path string, args []string, pluginName string, runtimeContext RuntimeContext, streams Stdio) (int, error) {
	command := exec.CommandContext(ctx, path, args...)
	command.Stdin, command.Stdout, command.Stderr = streams.Stdin, streams.Stdout, streams.Stderr
	environment, err := runtimeEnvironment(os.Environ(), runtimeContext, pluginName)
	if err != nil {
		return 1, err
	}
	if pluginName != "content" {
		command.Env = environment
		return waitPluginCommand(command.Run())
	}
	rootSocket, childSocket, err := newBrokerSocketPair()
	if err != nil {
		return 1, fmt.Errorf("create plugin broker: %w", err)
	}
	defer rootSocket.Close()
	command.Env = appendBrokerEnvironment(environment)
	command.ExtraFiles = []*os.File{childSocket}
	if err := command.Start(); err != nil {
		_ = childSocket.Close()
		return 1, err
	}
	_ = childSocket.Close()
	session := newBrokerSession(rootSocket, pluginName, runtimeContext)
	serveDone := make(chan error, 1)
	waitDone := make(chan error, 1)
	cancelDone := make(chan struct{})
	go func() { serveDone <- session.serve() }()
	go func() { waitDone <- command.Wait() }()
	go func() {
		select {
		case <-ctx.Done():
			session.cancel()
		case <-cancelDone:
		}
	}()
	select {
	case waitErr := <-waitDone:
		close(cancelDone)
		_ = session.close()
		<-serveDone
		return waitPluginCommand(waitErr)
	case serveErr := <-serveDone:
		close(cancelDone)
		if serveErr == nil {
			return waitPluginCommand(<-waitDone)
		}
		_ = command.Process.Kill()
		<-waitDone
		return 1, brokerProcessError(serveErr)
	}
}

func waitPluginCommand(err error) (int, error) {
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
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		key := entry
		if separator := strings.IndexByte(entry, '='); separator >= 0 {
			key = entry[:separator]
		}
		if allowedPluginEnvironment(key, "") {
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
	return s.Runner.Run(ctx, installed.Path, forwarded, definition.Name, runtimeContext, streams)
}

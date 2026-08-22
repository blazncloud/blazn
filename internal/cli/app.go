package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/auth"
	"github.com/blazncloud/blazn/internal/client"
	pluginpkg "github.com/blazncloud/blazn/internal/plugin"
	workspacepkg "github.com/blazncloud/blazn/internal/workspace"
)

const (
	ExitSuccess     = 0
	ExitFailure     = 1
	ExitUsage       = 2
	ExitUnavailable = 7
	ExitPartial     = 9
)

type OutputFormat string

const (
	OutputHuman OutputFormat = "human"
	OutputJSON  OutputFormat = "json"
)

type BuildInfo struct {
	Version         string `json:"version"`
	Commit          string `json:"commit"`
	BuildTime       string `json:"buildTime"`
	GOOS            string `json:"goos"`
	GOARCH          string `json:"goarch"`
	ContractVersion string `json:"contractVersion"`
}

type App struct {
	stdout      io.Writer
	stderr      io.Writer
	build       BuildInfo
	doctor      func() DoctorReport
	uninstall   func() (UninstallResult, error)
	auth        func() (authCommands, error)
	openBrowser func(string) error
	workspace   func() (workspaceCommands, error)
	node        func() (nodeCommands, error)
	stdin       io.Reader
	stdinTTY    func() bool
	plugins     pluginCommands
}

type pluginCommands interface {
	Resolve(string) (pluginpkg.Definition, bool)
	Installed(string) (pluginpkg.Installed, error)
	Install(context.Context, string) (pluginpkg.Receipt, error)
	List() []pluginpkg.Status
	Rollback(string) (pluginpkg.Receipt, error)
	Remove(string) error
	Run(context.Context, pluginpkg.Definition, []string, string, pluginpkg.Stdio) (int, error)
}

type authCommands interface {
	BeginLogin(context.Context) (auth.LoginStart, string, time.Duration, error)
	CompleteLogin(context.Context, string, time.Duration) (auth.LoginResult, error)
	Status(context.Context) (auth.StatusResult, error)
	Logout(context.Context) (auth.LogoutResult, error)
	Devices(context.Context) ([]client.Device, error)
	RevokeDevice(context.Context, string) error
}

func New(stdout, stderr io.Writer, build BuildInfo) *App {
	if build.GOOS == "" {
		build.GOOS = runtime.GOOS
	}
	if build.GOARCH == "" {
		build.GOARCH = runtime.GOARCH
	}
	pluginService, pluginErr := pluginpkg.NewService(build.Version)
	var plugins pluginCommands
	if pluginErr == nil {
		plugins = pluginService
	}
	return &App{
		stdout:    stdout,
		stderr:    stderr,
		build:     build,
		doctor:    func() DoctorReport { return RunDoctor(build) },
		uninstall: RunUninstall,
		auth: func() (authCommands, error) {
			return auth.NewDefaultService()
		},
		openBrowser: auth.OpenBrowser,
		workspace:   func() (workspaceCommands, error) { return workspacepkg.NewDefaultService() },
		node: func() (nodeCommands, error) {
			return nil, fmt.Errorf("node runtime requires an injected trusted local profile and privileged platform adapter")
		},
		stdin:    os.Stdin,
		stdinTTY: func() bool { info, err := os.Stdin.Stat(); return err == nil && info.Mode()&os.ModeCharDevice != 0 },
		plugins:  plugins,
	}
}

func (a *App) Run(args []string) int {
	format, positional, err := parseGlobalOptions(args)
	if err != nil {
		return a.writeError(format, ExitUsage, "usage", err.Error())
	}

	if len(positional) == 0 {
		return a.writeHelp(format, "")
	}

	command := positional[0]
	rest := positional[1:]
	switch command {
	case "help":
		if len(rest) > 1 {
			return a.writeError(format, ExitUsage, "usage", "help accepts at most one command name")
		}
		topic := ""
		if len(rest) == 1 {
			topic = rest[0]
		}
		return a.writeHelp(format, topic)
	case "-h", "--help":
		if len(rest) != 0 {
			return a.writeError(format, ExitUsage, "usage", "root help does not accept arguments")
		}
		return a.writeHelp(format, "")
	case "version":
		if helpRequested(rest) {
			return a.writeHelp(format, "version")
		}
		if len(rest) != 0 {
			return a.writeError(format, ExitUsage, "usage", "version does not accept arguments")
		}
		return a.writeVersion(format)
	case "doctor":
		if helpRequested(rest) {
			return a.writeHelp(format, "doctor")
		}
		if len(rest) != 0 {
			return a.writeError(format, ExitUsage, "usage", "doctor does not accept arguments")
		}
		return a.writeDoctor(format)
	case "uninstall":
		if helpRequested(rest) {
			return a.writeHelp(format, "uninstall")
		}
		if len(rest) != 1 || rest[0] != "--yes" {
			return a.writeError(format, ExitUsage, "confirmation_required", "uninstall requires --yes")
		}
		return a.writeUninstall(format)
	case "auth":
		return a.runAuth(format, rest)
	case "workspace":
		return a.runWorkspace(format, rest)
	case "node":
		return a.runNode(format, rest)
	case "plugins":
		return a.runPlugins(format, rest)
	default:
		if a.plugins != nil {
			if definition, ok := a.plugins.Resolve(command); ok {
				pluginArgs := positional
				if command == definition.CanonicalCommand {
					pluginArgs = rest
				}
				return a.runPlugin(format, definition, pluginArgs)
			}
		}
		return a.writeError(format, ExitUsage, "unknown_command", fmt.Sprintf("unknown command %q", command))
	}
}

func parseGlobalOptions(args []string) (OutputFormat, []string, error) {
	format := OutputHuman
	positional := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--output":
			if i+1 >= len(args) {
				return format, nil, fmt.Errorf("--output requires human or json")
			}
			i++
			value, err := outputFormat(args[i])
			if err != nil {
				return format, nil, err
			}
			format = value
		case strings.HasPrefix(arg, "--output="):
			value, err := outputFormat(strings.TrimPrefix(arg, "--output="))
			if err != nil {
				return format, nil, err
			}
			format = value
		default:
			positional = append(positional, arg)
		}
	}
	return format, positional, nil
}

func outputFormat(value string) (OutputFormat, error) {
	switch OutputFormat(value) {
	case OutputHuman, OutputJSON:
		return OutputFormat(value), nil
	default:
		return OutputHuman, fmt.Errorf("invalid --output value %q; expected human or json", value)
	}
}

func helpRequested(args []string) bool {
	return len(args) == 1 && (args[0] == "-h" || args[0] == "--help")
}

func (a *App) writeJSON(value any) int {
	encoder := json.NewEncoder(a.stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintf(a.stderr, "blazn: failed to write output: %v\n", err)
		return ExitFailure
	}
	return ExitSuccess
}

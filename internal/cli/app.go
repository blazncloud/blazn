package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/auth"
	"github.com/blazncloud/blazn/internal/client"
	nodepkg "github.com/blazncloud/blazn/internal/node"
	pluginpkg "github.com/blazncloud/blazn/internal/plugin"
	sandboxpkg "github.com/blazncloud/blazn/internal/sandbox"
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
	OutputJSONL OutputFormat = "jsonl"
	OutputCSV   OutputFormat = "csv"
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
	stdout        io.Writer
	stderr        io.Writer
	build         BuildInfo
	doctor        func() DoctorReport
	uninstall     func() (UninstallResult, error)
	auth          func() (authCommands, error)
	openBrowser   func(string) error
	workspace     func() (workspaceCommands, error)
	node          func(bool) (nodeCommands, error)
	sandbox       func() (sandboxCommands, error)
	stdin         io.Reader
	stdinTTY      func() bool
	plugins       pluginCommands
	pluginContext func(context.Context, OutputFormat) (pluginpkg.RuntimeContext, error)
}

type pluginCommands interface {
	Resolve(string) (pluginpkg.Definition, bool)
	Installed(string) (pluginpkg.Installed, error)
	Install(context.Context, string) (pluginpkg.Receipt, error)
	List() []pluginpkg.Status
	Rollback(string) (pluginpkg.Receipt, error)
	Remove(string) error
	Run(context.Context, pluginpkg.Definition, []string, string, pluginpkg.RuntimeContext, pluginpkg.Stdio) (int, error)
}

var defaultNodeCommandFactory = newDefaultNodeCommands

type authCommands interface {
	BeginLogin(context.Context) (auth.LoginStart, string, time.Duration, error)
	CompleteLogin(context.Context, string, time.Duration) (auth.LoginResult, error)
	Status(context.Context) (auth.StatusResult, error)
	Logout(context.Context) (auth.LogoutResult, error)
	Devices(context.Context) ([]client.Device, error)
	RevokeDevice(context.Context, string) error
}

type sandboxCommands interface {
	TemplateCommandRuntime
	SandboxCommandRuntime
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
	app := &App{
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
		node:        func(daemonOnly bool) (nodeCommands, error) { return defaultNodeCommandFactory(build, daemonOnly) },
		sandbox:     func() (sandboxCommands, error) { return sandboxpkg.NewDefaultService() },
		stdin:       os.Stdin,
		stdinTTY:    func() bool { info, err := os.Stdin.Stat(); return err == nil && info.Mode()&os.ModeCharDevice != 0 },
		plugins:     plugins,
	}
	app.pluginContext = app.resolvePluginContext
	return app
}

func (a *App) resolvePluginContext(ctx context.Context, format OutputFormat) (pluginpkg.RuntimeContext, error) {
	runtimeContext, err := pluginpkg.NewRuntimeContext(a.build.Version, string(format))
	if err != nil {
		return pluginpkg.RuntimeContext{}, err
	}
	commands, err := a.workspace()
	if err != nil {
		return runtimeContext, nil
	}
	selection, err := commands.CurrentSelection(ctx)
	runtimeContext.APIOrigin = selection.APIOrigin
	if err == nil {
		runtimeContext.Status = "selected"
		runtimeContext.ReasonCode = ""
		runtimeContext.UserID = selection.UserID
		runtimeContext.WorkspaceID = selection.WorkspaceID
		return runtimeContext, runtimeContext.Validate()
	}
	if errors.Is(err, workspacepkg.ErrNoContext) {
		runtimeContext.Status = "unselected"
		runtimeContext.ReasonCode = "workspace_not_selected"
	}
	return runtimeContext, runtimeContext.Validate()
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
	if format == OutputJSONL || format == OutputCSV {
		if a.plugins != nil {
			if definition, ok := a.plugins.Resolve(command); ok {
				pluginArgs := positional
				if command == definition.CanonicalCommand {
					pluginArgs = rest
				}
				return a.runPlugin(format, definition, pluginArgs)
			}
		}
		return a.writeError(OutputHuman, ExitUsage, "usage", fmt.Sprintf("--output %s is supported only by plugin commands", format))
	}
	switch command {
	case nodepkg.RootObserveSubcommand:
		if format != OutputHuman || len(rest) != 0 {
			return a.writeError(format, ExitUsage, "usage", "node root observer accepts no options")
		}
		if err := nodepkg.RunProductionObservationHelper(context.Background(), a.stdout); err != nil {
			fmt.Fprintln(a.stderr, "node root observation failed")
			return ExitFailure
		}
		return ExitSuccess
	case nodepkg.RootPrepareStateSubcommand:
		if format != OutputHuman || len(rest) != 0 {
			return a.writeError(format, ExitUsage, "usage", "node root state initializer accepts no options")
		}
		if err := nodepkg.PrepareProductionServiceState(); err != nil {
			fmt.Fprintln(a.stderr, "node root state initialization failed")
			return ExitFailure
		}
		return ExitSuccess
	case nodepkg.RootHelperSubcommand:
		if format != OutputHuman || len(rest) != 0 {
			return a.writeError(format, ExitUsage, "usage", "node root helper accepts no options")
		}
		if err := nodepkg.RunProductionRootHelper(context.Background(), a.stdin, a.stdout); err != nil {
			fmt.Fprintln(a.stderr, "node root helper failed")
			return ExitFailure
		}
		return ExitSuccess
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
	case "template":
		if helpRequested(rest) {
			return a.writeHelp(format, "template")
		}
		if len(rest) > 0 && rest[0] == "validate" {
			return a.RunTemplateCommand(context.Background(), format, rest, nil)
		}
		runtime, err := a.sandbox()
		if err != nil {
			return writeSandboxCLIError(format, a.stderr, a.stdout, ExitUnavailable, "unavailable", "sandbox command runtime is unavailable", "local")
		}
		return a.RunTemplateCommand(context.Background(), format, rest, runtime)
	case "sandbox":
		if helpRequested(rest) {
			return a.writeHelp(format, "sandbox")
		}
		runtime, err := a.sandbox()
		if err != nil {
			return writeSandboxCLIError(format, a.stderr, a.stdout, ExitUnavailable, "unavailable", "sandbox command runtime is unavailable", "local")
		}
		return a.RunSandboxCommand(context.Background(), format, rest, runtime)
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
				return format, nil, fmt.Errorf("--output requires human, json, jsonl, or csv")
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
	case OutputHuman, OutputJSON, OutputJSONL, OutputCSV:
		return OutputFormat(value), nil
	default:
		return OutputHuman, fmt.Errorf("invalid --output value %q; expected human, json, jsonl, or csv", value)
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

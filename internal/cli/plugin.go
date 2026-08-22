package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	pluginpkg "github.com/blazncloud/blazn/internal/plugin"
)

type pluginListOutput struct {
	Command string             `json:"command"`
	Plugins []pluginpkg.Status `json:"plugins"`
}

type pluginMutationOutput struct {
	Command string `json:"command"`
	Plugin  string `json:"plugin"`
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
}

func (a *App) runPlugin(format OutputFormat, definition pluginpkg.Definition, args []string) int {
	aliasHelp := len(args) == 2 && args[0] != definition.CanonicalCommand && helpRequested(args[1:])
	if helpRequested(args) || aliasHelp {
		return a.writeHelp(format, definition.CanonicalCommand)
	}
	_, err := a.plugins.Installed(definition.Name)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return a.writeError(format, ExitUnavailable, "plugin_unhealthy", err.Error())
		}
		if format != OutputHuman || !a.stdinTTY() {
			return a.writeError(format, ExitUnavailable, "plugin_required", fmt.Sprintf("command requires the %s plugin; install with 'blazn plugins install %s --yes'", definition.Name, definition.Name))
		}
		fmt.Fprintf(a.stderr, "The command requires the %s plugin.\nInstall %s from %s? [y/N] ", definition.Name, definition.Name, definition.Repository)
		var answer string
		if _, scanErr := fmt.Fscan(a.stdin, &answer); scanErr != nil && !errors.Is(scanErr, io.EOF) {
			return a.writeError(format, ExitFailure, "prompt_failed", "could not read plugin installation confirmation")
		}
		if strings.ToLower(strings.TrimSpace(answer)) != "y" && strings.ToLower(strings.TrimSpace(answer)) != "yes" {
			fmt.Fprintf(a.stderr, "Not installed. Install later with: blazn plugins install %s --yes\n", definition.Name)
			return ExitUnavailable
		}
		if _, err := a.plugins.Install(context.Background(), definition.Name); err != nil {
			return a.writeError(format, ExitUnavailable, "plugin_install_failed", err.Error())
		}
		fmt.Fprintf(a.stderr, "Installed %s. Continuing original command.\n", definition.Name)
	}
	ctx := context.Background()
	runtimeContext, err := a.pluginContext(ctx, format)
	if err != nil {
		return a.writeError(format, ExitUnavailable, "plugin_context_failed", err.Error())
	}
	code, err := a.plugins.Run(ctx, definition, args, string(format), runtimeContext, pluginpkg.Stdio{Stdin: a.stdin, Stdout: a.stdout, Stderr: a.stderr})
	if err != nil {
		return a.writeError(format, ExitUnavailable, "plugin_execution_failed", err.Error())
	}
	return code
}

func (a *App) runPlugins(format OutputFormat, args []string) int {
	if a.plugins == nil {
		return a.writeError(format, ExitUnavailable, "plugin_subsystem_unavailable", "plugin storage could not be initialized for the current user")
	}
	if len(args) == 0 || helpRequested(args) {
		return a.writeHelp(format, "plugins")
	}
	switch args[0] {
	case "list", "doctor":
		if len(args) != 1 {
			return a.writeError(format, ExitUsage, "usage", "plugins "+args[0]+" does not accept arguments")
		}
		statuses := a.plugins.List()
		if format == OutputJSON {
			return a.writeJSON(pluginListOutput{Command: "plugins " + args[0], Plugins: statuses})
		}
		for _, status := range statuses {
			state := "not installed"
			if status.Installed && status.Healthy {
				state = "installed " + status.Version
			}
			if status.Installed && !status.Healthy {
				state = "unhealthy: " + status.Message
			}
			fmt.Fprintf(a.stdout, "%s\t%s\t%s\n", status.Name, state, strings.Join(status.Commands, ","))
		}
		if args[0] == "doctor" {
			for _, status := range statuses {
				if status.Installed && !status.Healthy {
					return ExitUnavailable
				}
			}
		}
		return ExitSuccess
	case "install":
		name, ok := confirmedPluginMutation(args)
		if !ok {
			return a.writeError(format, ExitUsage, "confirmation_required", "plugins install requires NAME --yes")
		}
		receipt, err := a.plugins.Install(context.Background(), name)
		if err != nil {
			return a.writeError(format, ExitUnavailable, "plugin_install_failed", err.Error())
		}
		return a.writePluginMutation(format, pluginMutationOutput{Command: "plugins install", Plugin: name, Status: "installed", Version: receipt.Version})
	case "rollback":
		name, ok := confirmedPluginMutation(args)
		if !ok {
			return a.writeError(format, ExitUsage, "confirmation_required", "plugins rollback requires NAME --yes")
		}
		receipt, err := a.plugins.Rollback(name)
		if err != nil {
			return a.writeError(format, ExitUnavailable, "plugin_rollback_failed", err.Error())
		}
		return a.writePluginMutation(format, pluginMutationOutput{Command: "plugins rollback", Plugin: name, Status: "rolled_back", Version: receipt.Version})
	case "remove":
		name, ok := confirmedPluginMutation(args)
		if !ok {
			return a.writeError(format, ExitUsage, "confirmation_required", "plugins remove requires NAME --yes")
		}
		if err := a.plugins.Remove(name); err != nil {
			return a.writeError(format, ExitUnavailable, "plugin_remove_failed", err.Error())
		}
		return a.writePluginMutation(format, pluginMutationOutput{Command: "plugins remove", Plugin: name, Status: "removed"})
	default:
		return a.writeError(format, ExitUsage, "unknown_command", fmt.Sprintf("unknown plugins command %q", args[0]))
	}
}

func confirmedPluginMutation(args []string) (string, bool) {
	returnValue := ""
	confirmed := false
	for _, arg := range args[1:] {
		if arg == "--yes" {
			confirmed = true
		} else if returnValue == "" {
			returnValue = arg
		} else {
			return "", false
		}
	}
	return returnValue, confirmed && returnValue != ""
}

func (a *App) writePluginMutation(format OutputFormat, output pluginMutationOutput) int {
	if format == OutputJSON {
		return a.writeJSON(output)
	}
	if output.Version != "" {
		fmt.Fprintf(a.stdout, "%s %s %s\n", output.Status, output.Plugin, output.Version)
	} else {
		fmt.Fprintf(a.stdout, "%s %s\n", output.Status, output.Plugin)
	}
	return ExitSuccess
}

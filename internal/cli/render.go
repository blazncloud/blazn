package cli

import (
	"encoding/csv"
	"fmt"
	"strconv"
)

type helpOutput struct {
	Command  string        `json:"command"`
	Usage    string        `json:"usage"`
	Summary  string        `json:"summary"`
	Commands []helpCommand `json:"commands"`
}

type helpCommand struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
}

type errorOutput struct {
	Error    commandError `json:"error"`
	ExitCode int          `json:"exitCode"`
}

type commandError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

var rootCommands = []helpCommand{
	{Name: "auth", Summary: "Authenticate this device and manage sessions"},
	{Name: "content", Summary: "Manage Content media workflows (plugin)"},
	{Name: "doctor", Summary: "Run offline readiness checks"},
	{Name: "dev", Summary: "Validate, build, test, inspect, and publish development projects"},
	{Name: "help", Summary: "Show help for a command"},
	{Name: "node", Summary: "Enroll, install, recover, and heartbeat a Node"},
	{Name: "plugins", Summary: "Install and manage signed Blazn plugins"},
	{Name: "project", Summary: "Create, select, and manage Workspace Projects"},
	{Name: "proxy", Summary: "Activate, inspect, and recover local model routing"},
	{Name: "sandbox", Summary: "Create and operate isolated agent sandboxes"},
	{Name: "social", Summary: "Search public entities and manage social content (plugin)"},
	{Name: "template", Summary: "Validate and publish sandbox templates"},
	{Name: "uninstall", Summary: "Remove a receipt-owned direct installation"},
	{Name: "version", Summary: "Show build and contract version information"},
	{Name: "workspace", Summary: "Create, select, and manage workspaces"},
}

func (a *App) writeHelp(format OutputFormat, topic string) int {
	var output helpOutput
	switch topic {
	case "":
		output = helpOutput{
			Command:  "blazn",
			Usage:    "blazn [--output human|json] <command>",
			Summary:  "Control Blazn from the command line.",
			Commands: rootCommands,
		}
	case "help":
		output = helpOutput{Command: "help", Usage: "blazn help [command]", Summary: "Show root or command help."}
	case "version":
		output = helpOutput{Command: "version", Usage: "blazn version [--output human|json]", Summary: "Show build and contract version information."}
	case "doctor":
		output = helpOutput{Command: "doctor", Usage: "blazn doctor [--output human|json]", Summary: "Run deterministic checks without network access."}
	case "dev":
		output = helpOutput{Command: "dev", Usage: "blazn dev validate|build|status [options]", Summary: "Operate the available Development build workflow.", Commands: []helpCommand{{Name: "validate", Summary: "Validate a DevelopmentProject offline"}, {Name: "build", Summary: "Request an immutable multi-architecture Build"}, {Name: "status", Summary: "Get Build status"}}}
	case "uninstall":
		output = helpOutput{Command: "uninstall", Usage: "blazn uninstall --yes [--output human|json]", Summary: "Remove a direct installation owned by its Blazn receipt while preserving configuration."}
	case "auth":
		output = helpOutput{
			Command: "auth",
			Usage:   "blazn auth login|status|logout|devices|revoke-device [DEVICE_ID]",
			Summary: "Authenticate this device and manage its Blazn sessions.",
			Commands: []helpCommand{
				{Name: "login", Summary: "Authenticate using the browser or headless device flow"},
				{Name: "status", Summary: "Show the current authenticated user and device"},
				{Name: "logout", Summary: "Revoke and remove this device session"},
				{Name: "devices", Summary: "List devices for the current user"},
				{Name: "revoke-device", Summary: "Revoke a device by ID"},
			},
		}
	case "workspace":
		output = helpOutput{
			Command: "workspace", Usage: "blazn workspace <command> [--workspace WORKSPACE] [--request-id KEY (required for mutations)]", Summary: "Create, select, and manage Blazn workspaces.",
			Commands: []helpCommand{
				{Name: "create", Summary: "Create a workspace"}, {Name: "list", Summary: "List accessible workspaces"},
				{Name: "get", Summary: "Get a workspace"}, {Name: "edit", Summary: "Edit a workspace"}, {Name: "use", Summary: "Select a workspace locally"},
				{Name: "invite", Summary: "Create a one-time invitation"}, {Name: "invitations", Summary: "List invitations without tokens"}, {Name: "revoke-invite", Summary: "Revoke an invitation"},
				{Name: "join", Summary: "Accept an invitation from stdin"}, {Name: "members", Summary: "List workspace members"},
				{Name: "set-role", Summary: "Change a member role"}, {Name: "remove-member", Summary: "Remove a member"}, {Name: "leave", Summary: "Leave the selected workspace"},
				{Name: "watch", Summary: "Stream reauthorized workspace events"},
			},
		}
	case "project":
		output = helpOutput{
			Command: "project", Usage: "blazn project create|list|get|use|edit [options]", Summary: "Create, select, and manage Projects in the selected Workspace.",
			Commands: []helpCommand{
				{Name: "create", Summary: "Create a Project in the selected Workspace"},
				{Name: "list", Summary: "List Projects in the selected Workspace"},
				{Name: "get", Summary: "Get a Project by ID, slug, or current selection"},
				{Name: "use", Summary: "Select a Project locally"},
				{Name: "edit", Summary: "Update or archive a Project"},
			},
		}
	case "node":
		output = helpOutput{Command: "node", Usage: "blazn node list|get|capacity|enroll|recover|repair|uninstall|heartbeat|serve [options]", Summary: "Operate Nodes and their signed install/daemon runtime.", Commands: []helpCommand{{Name: "list", Summary: "List Nodes in a Workspace"}, {Name: "get", Summary: "Get a Node"}, {Name: "capacity", Summary: "Show Node eligibility and capability publication state"}, {Name: "enroll", Summary: "Enroll, root-authorize, and transactionally install this host"}, {Name: "recover", Summary: "Resume rollback from the install WAL"}, {Name: "repair", Summary: "Reconcile an active receipt using a current authorized plan"}, {Name: "uninstall", Summary: "Remove Node-owned state and restore receipt-captured prior values"}, {Name: "heartbeat", Summary: "Submit one node-proof capability heartbeat"}, {Name: "serve", Summary: "Run the token-free Node heartbeat daemon"}}}
	case "template":
		output = helpOutput{Command: "template", Usage: "blazn template validate|publish -f FILE [options]", Summary: "Validate templates offline or publish them to a workspace.", Commands: []helpCommand{{Name: "validate", Summary: "Validate and digest a local template without authentication"}, {Name: "publish", Summary: "Publish a valid template to a workspace"}}}
	case "sandbox":
		output = helpOutput{Command: "sandbox", Usage: "blazn sandbox create|list|get|watch|exec|upload|download|stop|delete [options]", Summary: "Create and operate isolated agent sandboxes.", Commands: []helpCommand{{Name: "create", Summary: "Request a sandbox from a published template"}, {Name: "list", Summary: "List sandboxes in a workspace"}, {Name: "get", Summary: "Get one sandbox"}, {Name: "watch", Summary: "Stream sandbox events as JSON Lines"}, {Name: "exec", Summary: "Run a command with a one-time access grant"}, {Name: "upload", Summary: "Upload a file with a one-time access grant"}, {Name: "download", Summary: "Download a file with a one-time access grant"}, {Name: "stop", Summary: "Request sandbox shutdown"}, {Name: "delete", Summary: "Request sandbox deletion"}}}
	case "proxy":
		output = helpOutput{Command: "proxy", Usage: "blazn proxy on|off|status|doctor|routes|tail|run|reset [options]", Summary: "Activate and recover authenticated loopback model routing.", Commands: []helpCommand{{Name: "on", Summary: "Prepare and activate a journaled session listener"}, {Name: "off", Summary: "Restore exact prior state without network access"}, {Name: "status", Summary: "Reconcile protected activation records"}, {Name: "doctor", Summary: "Validate policy, routes, credentials, and listener readiness"}, {Name: "routes", Summary: "List redacted policy routes"}, {Name: "tail", Summary: "Read redacted operational events"}, {Name: "run", Summary: "Run exact argv with a scoped listener"}, {Name: "reset", Summary: "Recover receipt-owned state after confirmation"}}}
	case "plugins":
		output = helpOutput{Command: "plugins", Usage: "blazn plugins list|doctor|install|rollback|remove [NAME] [--yes]", Summary: "Install and manage signed allowlisted Blazn plugins.", Commands: []helpCommand{{Name: "list", Summary: "List allowlisted plugins"}, {Name: "doctor", Summary: "Validate installed plugin receipts"}, {Name: "install", Summary: "Install a signed plugin release"}, {Name: "rollback", Summary: "Activate the previous installed version"}, {Name: "remove", Summary: "Remove a receipt-owned plugin"}}}
	default:
		if a.plugins != nil {
			if definition, ok := a.plugins.Resolve(topic); ok {
				commands := make([]helpCommand, 0, len(definition.Aliases))
				for _, alias := range definition.Aliases {
					commands = append(commands, helpCommand{Name: alias, Summary: "Run the " + alias + " command through " + definition.Name})
				}
				output = helpOutput{Command: definition.CanonicalCommand, Usage: "blazn " + definition.CanonicalCommand + " <command> [options]", Summary: "Commands provided by the signed " + definition.Name + " plugin.", Commands: commands}
				break
			}
		}
		return a.writeError(format, ExitUsage, "unknown_command", fmt.Sprintf("unknown help topic %q", topic))
	}

	if format == OutputJSON || format == OutputJSONL {
		return a.writeJSON(output)
	}
	if format == OutputCSV {
		writer := csv.NewWriter(a.stdout)
		_ = writer.Write([]string{"command", "usage", "summary", "subcommand", "subcommand_summary"})
		if len(output.Commands) == 0 {
			_ = writer.Write([]string{output.Command, output.Usage, output.Summary, "", ""})
		} else {
			for _, command := range output.Commands {
				_ = writer.Write([]string{output.Command, output.Usage, output.Summary, command.Name, command.Summary})
			}
		}
		writer.Flush()
		if err := writer.Error(); err != nil {
			fmt.Fprintf(a.stderr, "blazn: failed to write output: %v\n", err)
			return ExitFailure
		}
		return ExitSuccess
	}

	fmt.Fprintln(a.stdout, output.Summary)
	fmt.Fprintln(a.stdout)
	fmt.Fprintf(a.stdout, "Usage:\n  %s\n", output.Usage)
	if len(output.Commands) > 0 {
		fmt.Fprintln(a.stdout, "\nCommands:")
		for _, command := range output.Commands {
			fmt.Fprintf(a.stdout, "  %-9s %s\n", command.Name, command.Summary)
		}
	}
	return ExitSuccess
}

func (a *App) writeVersion(format OutputFormat) int {
	if format == OutputJSON || format == OutputJSONL {
		return a.writeJSON(a.build)
	}
	fmt.Fprintf(a.stdout, "blazn %s\n", a.build.Version)
	fmt.Fprintf(a.stdout, "commit: %s\n", a.build.Commit)
	fmt.Fprintf(a.stdout, "build time: %s\n", a.build.BuildTime)
	fmt.Fprintf(a.stdout, "platform: %s/%s\n", a.build.GOOS, a.build.GOARCH)
	fmt.Fprintf(a.stdout, "contract: %s\n", a.build.ContractVersion)
	return ExitSuccess
}

func (a *App) writeError(format OutputFormat, exitCode int, code, message string) int {
	if format == OutputJSON || format == OutputJSONL {
		if result := a.writeJSON(errorOutput{Error: commandError{Code: code, Message: message}, ExitCode: exitCode}); result != ExitSuccess {
			return result
		}
		return exitCode
	}
	if format == OutputCSV {
		writer := csv.NewWriter(a.stdout)
		_ = writer.Write([]string{"status", "error_code", "error_message", "exit_code"})
		_ = writer.Write([]string{"error", code, message, strconv.Itoa(exitCode)})
		writer.Flush()
		if err := writer.Error(); err != nil {
			fmt.Fprintf(a.stderr, "blazn: failed to write output: %v\n", err)
			return ExitFailure
		}
		return exitCode
	}
	fmt.Fprintf(a.stderr, "blazn: %s\n", message)
	fmt.Fprintln(a.stderr, "Run 'blazn help' for usage.")
	return exitCode
}

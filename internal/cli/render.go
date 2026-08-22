package cli

import "fmt"

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
	{Name: "doctor", Summary: "Run offline readiness checks"},
	{Name: "help", Summary: "Show help for a command"},
	{Name: "uninstall", Summary: "Remove a receipt-owned direct installation"},
	{Name: "version", Summary: "Show build and contract version information"},
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
	default:
		return a.writeError(format, ExitUsage, "unknown_command", fmt.Sprintf("unknown help topic %q", topic))
	}

	if format == OutputJSON {
		return a.writeJSON(output)
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
	if format == OutputJSON {
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
	if format == OutputJSON {
		if result := a.writeJSON(errorOutput{Error: commandError{Code: code, Message: message}, ExitCode: exitCode}); result != ExitSuccess {
			return result
		}
		return exitCode
	}
	fmt.Fprintf(a.stderr, "blazn: %s\n", message)
	fmt.Fprintln(a.stderr, "Run 'blazn help' for usage.")
	return exitCode
}

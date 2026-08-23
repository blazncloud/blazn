package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/proxy/activation"
)

type proxyCommands interface {
	On(context.Context, string, string) (activation.Result, error)
	Off(context.Context, bool) (activation.Result, error)
	Status(context.Context) (activation.Result, error)
	Doctor(context.Context, string) (activation.Result, error)
	Routes(context.Context, string) ([]activation.Route, error)
	Tail(context.Context, string, bool) ([]activation.Event, error)
	Run(context.Context, string, []string) (activation.Result, error)
	Reset(context.Context, bool, bool) (activation.Result, error)
}

var defaultProxyCommandFactory = func() (proxyCommands, error) { return nil, activation.ErrUnavailable }

func (a *App) runProxy(format OutputFormat, args []string) int {
	if len(args) == 0 || helpRequested(args) {
		return a.writeHelp(format, "proxy")
	}
	if format == OutputJSONL && args[0] != "tail" {
		return a.proxyUsage(OutputHuman, "--output jsonl is supported only by proxy tail")
	}
	runtime, err := a.proxy()
	if err != nil {
		return a.writeError(format, ExitUnavailable, "PROXY_PLATFORM_UNAVAILABLE", "proxy platform adapter is unavailable")
	}
	ctx := context.Background()
	switch args[0] {
	case "on":
		values, err := proxyFlags(args[1:], map[string]bool{"policy": false, "mode": false})
		if err != nil || values.flags["policy"] == "" || len(values.positionals) != 0 {
			return a.proxyUsage(format, "proxy on requires --policy POLICY [--mode auto|session]")
		}
		mode := values.flags["mode"]
		if mode == "" {
			mode = "auto"
		}
		if mode != "auto" && mode != "session" {
			return a.proxyUsage(format, "proxy on --mode must be auto or session")
		}
		result, callErr := runtime.On(ctx, values.flags["policy"], mode)
		return a.writeProxyResult(format, result, callErr)
	case "off":
		values, err := proxyFlags(args[1:], map[string]bool{"remove-ca": true})
		if err != nil || len(values.positionals) != 0 {
			return a.proxyUsage(format, "proxy off accepts only --remove-ca")
		}
		result, callErr := runtime.Off(ctx, values.present["remove-ca"])
		return a.writeProxyResult(format, result, callErr)
	case "status":
		if len(args) != 1 {
			return a.proxyUsage(format, "proxy status accepts no arguments")
		}
		result, callErr := runtime.Status(ctx)
		return a.writeProxyResult(format, result, callErr)
	case "doctor":
		values, err := proxyFlags(args[1:], map[string]bool{"policy": false})
		if err != nil || values.flags["policy"] == "" || len(values.positionals) != 0 {
			return a.proxyUsage(format, "proxy doctor requires --policy POLICY")
		}
		result, callErr := runtime.Doctor(ctx, values.flags["policy"])
		return a.writeProxyResult(format, result, callErr)
	case "routes":
		values, err := proxyFlags(args[1:], map[string]bool{"policy": false})
		if err != nil || values.flags["policy"] == "" || len(values.positionals) != 0 {
			return a.proxyUsage(format, "proxy routes requires --policy POLICY")
		}
		routes, callErr := runtime.Routes(ctx, values.flags["policy"])
		if callErr != nil {
			return a.writeProxyError(format, callErr, 1)
		}
		if format == OutputJSON {
			return a.writeJSON(map[string]any{"command": "proxy routes", "contractVersion": activation.ContractVersion, "status": "success", "state": "inactive", "timestamp": time.Now().UTC().Format(time.RFC3339), "exitCode": 0, "routes": routes})
		}
		for _, route := range routes {
			fmt.Fprintf(a.stdout, "%-24s %-14s %-18s %s\n", route.ID, route.DestinationClass, route.DestinationProtocol, route.Endpoint)
		}
		return ExitSuccess
	case "tail":
		values, err := proxyFlags(args[1:], map[string]bool{"cursor": false, "follow": true})
		if err != nil || len(values.positionals) != 0 {
			return a.proxyUsage(format, "proxy tail accepts [--cursor CURSOR] [--follow]")
		}
		events, callErr := runtime.Tail(ctx, values.flags["cursor"], values.present["follow"])
		if callErr != nil {
			return a.writeProxyError(format, callErr, 1)
		}
		if format == OutputJSONL {
			for _, event := range events {
				if code := a.writeJSON(event); code != 0 {
					return code
				}
			}
			return 0
		}
		if format == OutputJSON {
			return a.writeJSON(map[string]any{"command": "proxy tail", "contractVersion": activation.ContractVersion, "status": "success", "state": "active", "timestamp": time.Now().UTC().Format(time.RFC3339), "exitCode": 0, "events": events})
		}
		for _, event := range events {
			fmt.Fprintf(a.stdout, "%s %s %s %s\n", event.Cursor, event.Type, event.Outcome, event.ReasonCode)
		}
		return ExitSuccess
	case "run":
		separator := -1
		for index, value := range args[1:] {
			if value == "--" {
				separator = index + 1
				break
			}
		}
		if separator < 0 || separator+1 >= len(args) {
			return a.proxyUsage(format, "proxy run requires --policy POLICY -- COMMAND...")
		}
		values, err := proxyFlags(args[1:separator], map[string]bool{"policy": false})
		if err != nil || values.flags["policy"] == "" || len(values.positionals) != 0 {
			return a.proxyUsage(format, "proxy run requires --policy POLICY -- COMMAND...")
		}
		result, callErr := runtime.Run(ctx, values.flags["policy"], args[separator+1:])
		return a.writeProxyResult(format, result, callErr)
	case "reset":
		values, err := proxyFlags(args[1:], map[string]bool{"yes": true, "remove-ca": true})
		if err != nil || len(values.positionals) != 0 || !values.present["yes"] {
			return a.proxyUsage(format, "proxy reset requires --yes [--remove-ca]")
		}
		result, callErr := runtime.Reset(ctx, true, values.present["remove-ca"])
		return a.writeProxyResult(format, result, callErr)
	default:
		return a.proxyUsage(format, fmt.Sprintf("unknown proxy command %q", args[0]))
	}
}

type parsedProxyFlags struct {
	flags       map[string]string
	present     map[string]bool
	positionals []string
}

func proxyFlags(args []string, allowed map[string]bool) (parsedProxyFlags, error) {
	result := parsedProxyFlags{flags: map[string]string{}, present: map[string]bool{}}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "--") {
			result.positionals = append(result.positionals, arg)
			continue
		}
		name := strings.TrimPrefix(arg, "--")
		value := ""
		if split := strings.IndexByte(name, '='); split >= 0 {
			value, name = name[split+1:], name[:split]
		}
		boolean, ok := allowed[name]
		if !ok || result.present[name] {
			return result, errors.New("unknown or duplicate proxy flag")
		}
		if !boolean && value == "" {
			if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") {
				return result, errors.New("proxy flag requires a value")
			}
			index++
			value = args[index]
		}
		if boolean && value != "" {
			return result, errors.New("boolean proxy flag does not accept a value")
		}
		result.flags[name] = value
		result.present[name] = true
	}
	return result, nil
}
func (a *App) proxyUsage(format OutputFormat, message any) int {
	return a.writeError(format, ExitUsage, "usage", fmt.Sprint(message))
}
func (a *App) writeProxyResult(format OutputFormat, result activation.Result, err error) int {
	if err != nil {
		return a.writeProxyError(format, err, result.ExitCode)
	}
	if format == OutputJSON {
		if code := a.writeJSON(result); code != ExitSuccess {
			return code
		}
		return result.ExitCode
	}
	fmt.Fprintf(a.stdout, "%s: %s (%s)\n", result.Command, result.Status, result.State)
	for _, line := range result.ManualRemediation {
		fmt.Fprintln(a.stdout, line)
	}
	return result.ExitCode
}
func (a *App) writeProxyError(format OutputFormat, err error, suggested int) int {
	exit := suggested
	if exit == 0 {
		exit = 1
	}
	code := "PROXY_FAILED"
	message := "proxy operation failed"
	switch {
	case errors.Is(err, activation.ErrDifferentPolicy):
		code = "PROXY_ALREADY_ACTIVE_DIFFERENT_POLICY"
		message = "proxy is already active with a different policy"
		exit = 6
	case errors.Is(err, activation.ErrDifferentScope):
		code = "PROXY_ALREADY_ACTIVE_DIFFERENT_SCOPE"
		message = "proxy is already active in a different mode or OS session"
		exit = 6
	case errors.Is(err, activation.ErrRecovery):
		code = "RECOVERY_REQUIRED"
		message = "proxy recovery is required"
		exit = 9
	case errors.Is(err, activation.ErrUnavailable):
		code = "PROXY_PLATFORM_UNAVAILABLE"
		message = "proxy platform adapter is unavailable"
		exit = 7
	case errors.Is(err, activation.ErrCARemovalUnsupported):
		code = "PROXY_CA_REMOVAL_UNSUPPORTED"
		message = "proxy CA removal is unsupported"
		exit = 7
	}
	return a.writeError(format, exit, code, message)
}

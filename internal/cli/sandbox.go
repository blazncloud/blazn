package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/client"
	sandboxpkg "github.com/blazncloud/blazn/internal/sandbox"
)

type SandboxCommandRuntime interface {
	Create(context.Context, string, string, client.CreateSandboxRequest) (client.SandboxMutation, error)
	List(context.Context, string, string) (client.SandboxList, error)
	Get(context.Context, string) (client.Sandbox, error)
	Watch(context.Context, string, string, func(client.SandboxEvent) error) (sandboxpkg.WatchTerminal, error)
	Exec(context.Context, string, []string) (sandboxpkg.ExecResult, error)
	Upload(context.Context, string, string, string) (sandboxpkg.TransferResult, error)
	Download(context.Context, string, string, string) (sandboxpkg.TransferResult, error)
	Stop(context.Context, string, string) (client.SandboxMutation, error)
	Delete(context.Context, string, string) (client.SandboxMutation, error)
}

// RunSandboxCommand is the integration hook used by the root command wiring.
func (a *App) RunSandboxCommand(ctx context.Context, format OutputFormat, args []string, runtime SandboxCommandRuntime) int {
	return runSandboxCommand(ctx, format, args, runtime, a.stdout, a.stderr)
}

func runSandboxCommand(ctx context.Context, format OutputFormat, args []string, runtime SandboxCommandRuntime, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", "sandbox requires a command", "local")
	}
	if runtime == nil {
		return writeSandboxCLIError(format, stderr, stdout, ExitUnavailable, "unavailable", "sandbox command runtime is unavailable", "local")
	}
	switch args[0] {
	case "create":
		return runSandboxCreate(ctx, format, args[1:], runtime, stdout, stderr)
	case "list":
		return runSandboxList(ctx, format, args[1:], runtime, stdout, stderr)
	case "get":
		return runSandboxGet(ctx, format, args[1:], runtime, stdout, stderr)
	case "watch":
		return runSandboxWatch(ctx, args[1:], runtime, stdout, stderr)
	case "exec":
		return runSandboxExec(ctx, format, args[1:], runtime, stdout, stderr)
	case "upload":
		return runSandboxTransfer(ctx, format, "upload", args[1:], runtime, stdout, stderr)
	case "download":
		return runSandboxTransfer(ctx, format, "download", args[1:], runtime, stdout, stderr)
	case "stop", "delete":
		return runSandboxMutation(ctx, format, args[0], args[1:], runtime, stdout, stderr)
	default:
		return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", "unknown sandbox command", "local")
	}
}

func runSandboxCreate(ctx context.Context, format OutputFormat, args []string, runtime SandboxCommandRuntime, stdout, stderr io.Writer) int {
	allowed := map[string]flagKind{"template": flagValue, "arch": flagValue, "mode": flagValue, "expires": flagValue, "approved-non-sensitive": flagBool, "workspace": flagValue, "request-id": flagValue, "source": flagMulti}
	values, positional, err := parseSandboxFlags(args, allowed, false)
	if err != nil || len(positional) != 0 {
		if err == nil {
			err = errors.New("sandbox create accepts no positional arguments")
		}
		return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", err.Error(), "local")
	}
	required := []string{"template", "arch", "mode", "expires", "approved-non-sensitive", "workspace", "request-id"}
	for _, name := range required {
		if firstFlag(values, name) == "" {
			return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", "sandbox create requires --"+name, "local")
		}
	}
	if !validRequestID(firstFlag(values, "request-id")) {
		return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", "--request-id must contain 8 to 128 characters", "local")
	}
	template, err := parseTemplateReference(firstFlag(values, "template"))
	if err != nil {
		return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", err.Error(), "local")
	}
	arch := client.SandboxArchitecture(firstFlag(values, "arch"))
	if arch != client.SandboxAMD64 && arch != client.SandboxARM64 {
		return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", "--arch must be amd64 or arm64", "local")
	}
	mode := client.SandboxAllocationMode(firstFlag(values, "mode"))
	if mode != client.SandboxDirect && mode != client.SandboxClaim {
		return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", "--mode must be direct or claim", "local")
	}
	expires, err := parseExpiry(firstFlag(values, "expires"))
	if err != nil {
		return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", err.Error(), "local")
	}
	sources, err := parseSources(values["source"])
	if err != nil {
		return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", err.Error(), "local")
	}
	request := client.CreateSandboxRequest{Template: template, Architecture: arch, AllocationMode: mode, ExpiresInSeconds: expires, Sources: sources, ApprovedNonSensitive: true}
	result, err := runtime.Create(ctx, firstFlag(values, "workspace"), firstFlag(values, "request-id"), request)
	if err != nil {
		return writeSandboxRuntimeError(format, stdout, stderr, err)
	}
	return writeSandboxValue(format, stdout, stderr, result, "sandbox creation accepted")
}

func runSandboxList(ctx context.Context, format OutputFormat, args []string, runtime SandboxCommandRuntime, stdout, stderr io.Writer) int {
	values, positional, err := parseSandboxFlags(args, map[string]flagKind{"workspace": flagValue, "cursor": flagValue}, false)
	if err != nil || len(positional) != 0 || firstFlag(values, "workspace") == "" {
		if err == nil {
			err = errors.New("sandbox list requires --workspace and accepts optional --cursor")
		}
		return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", err.Error(), "local")
	}
	result, err := runtime.List(ctx, firstFlag(values, "workspace"), firstFlag(values, "cursor"))
	if err != nil {
		return writeSandboxRuntimeError(format, stdout, stderr, err)
	}
	return writeSandboxValue(format, stdout, stderr, result, fmt.Sprintf("%d sandbox(es)", len(result.Items)))
}

func runSandboxGet(ctx context.Context, format OutputFormat, args []string, runtime SandboxCommandRuntime, stdout, stderr io.Writer) int {
	values, positional, err := parseSandboxFlags(args, map[string]flagKind{}, false)
	_ = values
	if err != nil || len(positional) != 1 || !validSandboxID(positionalValue(positional)) {
		if err == nil {
			err = errors.New("sandbox get requires one SANDBOX_ID")
		}
		return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", err.Error(), "local")
	}
	result, err := runtime.Get(ctx, positional[0])
	if err != nil {
		return writeSandboxRuntimeError(format, stdout, stderr, err)
	}
	return writeSandboxValue(format, stdout, stderr, result, "sandbox "+result.ID)
}

func runSandboxWatch(ctx context.Context, args []string, runtime SandboxCommandRuntime, stdout, stderr io.Writer) int {
	values, positional, err := parseSandboxFlags(args, map[string]flagKind{"last-event-id": flagValue}, false)
	if err != nil || len(positional) != 1 || !validSandboxID(positionalValue(positional)) {
		if err == nil {
			err = errors.New("sandbox watch requires one SANDBOX_ID")
		}
		return writeSandboxCLIError(OutputJSON, stderr, stdout, ExitUsage, "usage", err.Error(), "local")
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	terminal, err := runtime.Watch(ctx, positional[0], firstFlag(values, "last-event-id"), func(event client.SandboxEvent) error { return encoder.Encode(event) })
	if err != nil {
		return writeSandboxRuntimeError(OutputJSON, stdout, stderr, err)
	}
	if terminal == sandboxpkg.WatchReady {
		return ExitSuccess
	}
	return ExitFailure
}

func runSandboxExec(ctx context.Context, format OutputFormat, args []string, runtime SandboxCommandRuntime, stdout, stderr io.Writer) int {
	delimiter := -1
	for i, arg := range args {
		if arg == "--" {
			delimiter = i
			break
		}
	}
	if delimiter != 1 || !validSandboxID(positionalValue(args[:maxInt(delimiter, 0)])) || delimiter == len(args)-1 {
		return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", "sandbox exec requires SANDBOX_ID -- COMMAND...", "local")
	}
	command := args[delimiter+1:]
	if len(command) > 32 {
		return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", "exec command may contain at most 32 arguments", "local")
	}
	for _, argument := range command {
		if argument == "" || len(argument) > 1024 {
			return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", "exec arguments must contain 1 to 1024 bytes", "local")
		}
	}
	result, err := runtime.Exec(ctx, args[0], command)
	if err == nil {
		return writeSandboxValue(format, stdout, stderr, result, "remote command completed")
	}
	if sandboxpkg.IsUnavailable(err) {
		return writeSandboxRuntimeError(format, stdout, stderr, err)
	}
	if sandboxpkg.IsPartial(err) {
		if format == OutputJSON {
			if code := encodeSandboxValue(stdout, stderr, result); code != ExitSuccess {
				return code
			}
		} else {
			fmt.Fprintln(stderr, "blazn: remote command evidence is incomplete")
		}
		return ExitPartial
	}
	var remote *sandboxpkg.RemoteExitError
	if errors.As(err, &remote) {
		if format == OutputJSON {
			if code := encodeSandboxValue(stdout, stderr, result); code != ExitSuccess {
				return code
			}
		} else {
			fmt.Fprintln(stderr, remote.Error())
		}
		return ExitFailure
	}
	return writeSandboxRuntimeError(format, stdout, stderr, err)
}

func runSandboxTransfer(ctx context.Context, format OutputFormat, command string, args []string, runtime SandboxCommandRuntime, stdout, stderr io.Writer) int {
	_, positional, err := parseSandboxFlags(args, map[string]flagKind{}, false)
	if err != nil || len(positional) != 3 || !validSandboxID(positionalValue(positional)) {
		if err == nil {
			err = fmt.Errorf("sandbox %s requires SANDBOX_ID SOURCE DESTINATION", command)
		}
		return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", err.Error(), "local")
	}
	var result sandboxpkg.TransferResult
	if command == "upload" {
		result, err = runtime.Upload(ctx, positional[0], positional[1], positional[2])
	} else {
		result, err = runtime.Download(ctx, positional[0], positional[1], positional[2])
	}
	if err != nil {
		return writeSandboxRuntimeError(format, stdout, stderr, err)
	}
	return writeSandboxValue(format, stdout, stderr, result, "sandbox file "+command+" completed")
}

func runSandboxMutation(ctx context.Context, format OutputFormat, command string, args []string, runtime SandboxCommandRuntime, stdout, stderr io.Writer) int {
	values, positional, err := parseSandboxFlags(args, map[string]flagKind{"request-id": flagValue}, false)
	if err != nil || len(positional) != 1 || !validSandboxID(positionalValue(positional)) || !validRequestID(firstFlag(values, "request-id")) {
		if err == nil {
			err = fmt.Errorf("sandbox %s requires SANDBOX_ID and an 8 to 128 character --request-id", command)
		}
		return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", err.Error(), "local")
	}
	var result client.SandboxMutation
	if command == "stop" {
		result, err = runtime.Stop(ctx, positional[0], firstFlag(values, "request-id"))
	} else {
		result, err = runtime.Delete(ctx, positional[0], firstFlag(values, "request-id"))
	}
	if err != nil {
		return writeSandboxRuntimeError(format, stdout, stderr, err)
	}
	return writeSandboxValue(format, stdout, stderr, result, "sandbox "+command+" accepted")
}

var (
	sandboxUUIDPattern  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)
	sourceCommitPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)
	templateNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
)

func validSandboxID(value string) bool { return sandboxUUIDPattern.MatchString(value) }
func positionalValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func parseTemplateReference(value string) (client.SandboxTemplateReference, error) {
	parts := strings.Split(value, "@")
	if len(parts) > 2 || !templateNamePattern.MatchString(parts[0]) {
		return client.SandboxTemplateReference{}, errors.New("--template must be NAME or NAME@VERSION")
	}
	ref := client.SandboxTemplateReference{Name: parts[0]}
	if len(parts) == 2 {
		if parts[1] == "" || len(parts[1]) > 128 {
			return ref, errors.New("template version is invalid")
		}
		ref.Version = parts[1]
	}
	return ref, nil
}
func parseExpiry(value string) (int64, error) {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds >= 60 && seconds <= 7200 {
			return seconds, nil
		}
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration%time.Second != 0 || duration < time.Minute || duration > 2*time.Hour {
		return 0, errors.New("--expires must be 60 to 7200 seconds (for example 30m)")
	}
	return int64(duration / time.Second), nil
}
func parseSources(values []string) ([]client.SandboxSource, error) {
	if len(values) > 8 {
		return nil, errors.New("--source may be supplied at most 8 times")
	}
	result, seen := make([]client.SandboxSource, 0, len(values)), map[string]bool{}
	for _, value := range values {
		parts := strings.SplitN(value, "=", 2)
		if len(parts) != 2 || !templateNamePattern.MatchString(parts[0]) || !sourceCommitPattern.MatchString(parts[1]) || seen[parts[0]] {
			return nil, errors.New("--source must be unique REPOSITORY=40_TO_64_LOWERCASE_HEX_COMMIT")
		}
		seen[parts[0]] = true
		result = append(result, client.SandboxSource{Repository: parts[0], Commit: parts[1]})
	}
	return result, nil
}

type sandboxCLIErrorEnvelope struct {
	Error    sandboxCLIErrorBody `json:"error"`
	ExitCode int                 `json:"exitCode"`
}
type sandboxCLIErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId"`
}

func writeSandboxRuntimeError(format OutputFormat, stdout, stderr io.Writer, err error) int {
	exit, code, message, requestID := ExitFailure, "sandbox_failed", err.Error(), "local"
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Body.Code != "" {
			code = apiErr.Body.Code
		}
		if apiErr.Body.Message != "" {
			message = apiErr.Body.Message
		}
		if apiErr.Body.RequestID != "" {
			requestID = apiErr.Body.RequestID
		}
	}
	if sandboxpkg.IsUnavailable(err) {
		exit = ExitUnavailable
		code = "unavailable"
	}
	return writeSandboxCLIError(format, stderr, stdout, exit, code, message, requestID)
}
func writeSandboxCLIError(format OutputFormat, stderr, stdout io.Writer, exit int, code, message, requestID string) int {
	if requestID == "" {
		requestID = "local"
	}
	if format == OutputJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(sandboxCLIErrorEnvelope{Error: sandboxCLIErrorBody{Code: code, Message: message, RequestID: requestID}, ExitCode: exit}); err != nil {
			fmt.Fprintln(stderr, "blazn: failed to write output")
			return ExitFailure
		}
		return exit
	}
	fmt.Fprintf(stderr, "blazn: %s\n", message)
	return exit
}
func writeSandboxValue(format OutputFormat, stdout, stderr io.Writer, value any, message string) int {
	if format == OutputJSON {
		return encodeSandboxValue(stdout, stderr, value)
	}
	fmt.Fprintln(stdout, message)
	return ExitSuccess
}
func encodeSandboxValue(stdout, stderr io.Writer, value any) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintln(stderr, "blazn: failed to write output")
		return ExitFailure
	}
	return ExitSuccess
}

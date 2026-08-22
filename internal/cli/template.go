package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	sandboxpkg "github.com/blazncloud/blazn/internal/sandbox"
)

type TemplateCommandRuntime interface {
	Publish(context.Context, string, string, []byte) (sandboxpkg.TemplatePublish, error)
}

// RunTemplateCommand is the integration hook used by the root command wiring.
func (a *App) RunTemplateCommand(ctx context.Context, format OutputFormat, args []string, runtime TemplateCommandRuntime) int {
	return runTemplateCommand(ctx, format, args, runtime, a.stdout, a.stderr)
}

func runTemplateCommand(ctx context.Context, format OutputFormat, args []string, runtime TemplateCommandRuntime, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", "template requires validate or publish", "local")
	}
	command := args[0]
	values, positional, err := parseSandboxFlags(args[1:], map[string]flagKind{"f": flagValue, "workspace": flagValue, "request-id": flagValue}, false)
	if err != nil {
		return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", err.Error(), "local")
	}
	if len(positional) != 0 {
		return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", "template commands accept no positional arguments", "local")
	}
	file := firstFlag(values, "f")
	if file == "" {
		return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", "template command requires -f FILE", "local")
	}
	manifest, err := sandboxpkg.ReadTemplateFile(file)
	if err != nil {
		return writeSandboxCLIError(format, stderr, stdout, ExitFailure, "template_invalid", err.Error(), "local")
	}
	switch command {
	case "validate":
		if firstFlag(values, "workspace") != "" || firstFlag(values, "request-id") != "" {
			return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", "template validate accepts only -f", "local")
		}
		result := sandboxpkg.ValidateTemplate(manifest)
		if format == OutputJSON {
			_ = json.NewEncoder(stdout).Encode(result)
		} else if result.Valid {
			fmt.Fprintf(stdout, "template is valid (%s)\n", *result.ManifestDigest)
		} else {
			for _, item := range result.Errors {
				fmt.Fprintf(stderr, "blazn: %s\n", item)
			}
		}
		if result.Valid {
			return ExitSuccess
		}
		return ExitFailure
	case "publish":
		workspace, requestID := firstFlag(values, "workspace"), firstFlag(values, "request-id")
		if workspace == "" || !validRequestID(requestID) {
			return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", "template publish requires --workspace and an 8 to 128 character --request-id", "local")
		}
		if runtime == nil {
			return writeSandboxCLIError(format, stderr, stdout, ExitUnavailable, "unavailable", "sandbox command runtime is unavailable", "local")
		}
		result, err := runtime.Publish(ctx, workspace, requestID, manifest)
		if err != nil {
			return writeSandboxRuntimeError(format, stdout, stderr, err)
		}
		return writeSandboxValue(format, stdout, stderr, result, "template published")
	default:
		return writeSandboxCLIError(format, stderr, stdout, ExitUsage, "usage", "unknown template command", "local")
	}
}

type flagKind uint8

const (
	flagValue flagKind = iota
	flagBool
	flagMulti
)

func parseSandboxFlags(args []string, allowed map[string]flagKind, stopAtDoubleDash bool) (map[string][]string, []string, error) {
	values, positional := map[string][]string{}, []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" && stopAtDoubleDash {
			return values, append(positional, args[i+1:]...), nil
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		if forbiddenSandboxOption(name) {
			return nil, nil, errors.New("secret-bearing access and grant options are forbidden")
		}
		value, hasEquals := "", false
		if split := strings.IndexByte(name, '='); split >= 0 {
			value, name, hasEquals = name[split+1:], name[:split], true
		}
		kind, ok := allowed[name]
		if !ok {
			return nil, nil, fmt.Errorf("unknown option --%s", name)
		}
		if kind == flagBool {
			if hasEquals {
				return nil, nil, fmt.Errorf("--%s does not accept a value", name)
			}
			value = "true"
		} else if !hasEquals {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return nil, nil, fmt.Errorf("--%s requires a value", name)
			}
			i++
			value = args[i]
		}
		if value == "" {
			return nil, nil, fmt.Errorf("--%s requires a non-empty value", name)
		}
		if kind != flagMulti && len(values[name]) != 0 {
			return nil, nil, fmt.Errorf("--%s may be supplied only once", name)
		}
		values[name] = append(values[name], value)
	}
	return values, positional, nil
}
func firstFlag(values map[string][]string, name string) string {
	if len(values[name]) == 0 {
		return ""
	}
	return values[name][0]
}
func forbiddenSandboxOption(name string) bool {
	lower := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(strings.SplitN(name, "=", 2)[0], "-", ""), "_", ""))
	return lower == "accesstoken" || lower == "granttoken" || lower == "bearertoken"
}
func validRequestID(value string) bool { return len(value) >= 8 && len(value) <= 128 }

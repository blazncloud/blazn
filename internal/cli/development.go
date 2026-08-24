package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/blazncloud/blazn/internal/auth"
	"github.com/blazncloud/blazn/internal/client"
	developmentpkg "github.com/blazncloud/blazn/internal/development"
	projectpkg "github.com/blazncloud/blazn/internal/project"
	workspacepkg "github.com/blazncloud/blazn/internal/workspace"
)

type developmentCommands interface {
	Build(context.Context, string, string) (developmentpkg.BuildDocument, error)
	Status(context.Context, string) (developmentpkg.BuildDocument, error)
}

func (a *App) runDevelopment(format OutputFormat, args []string) int {
	if len(args) == 0 || helpRequested(args) {
		return a.writeHelp(format, "dev")
	}
	command := args[0]
	if command == "validate" {
		values, positional, err := parseSandboxFlags(args[1:], map[string]flagKind{"f": flagValue}, false)
		if err != nil || len(positional) != 0 {
			return a.developmentUsage(format, "dev validate accepts only -f FILE")
		}
		file := firstFlag(values, "f")
		if file == "" {
			file = "blazn.yaml"
		}
		data, err := developmentpkg.ReadFile(file)
		if err != nil {
			return a.writeError(format, ExitFailure, "development_project_invalid", err.Error())
		}
		result, _ := developmentpkg.Validate(data)
		if format == OutputJSON {
			if code := a.writeJSON(result); code != ExitSuccess {
				return code
			}
			if !result.Valid {
				return ExitFailure
			}
			return ExitSuccess
		}
		if result.Valid {
			fmt.Fprintf(a.stdout, "DevelopmentProject is valid (%s)\n", *result.ManifestDigest)
			return ExitSuccess
		}
		for _, message := range result.Errors {
			fmt.Fprintf(a.stderr, "blazn: %s\n", message)
		}
		return ExitFailure
	}
	ctx := context.Background()
	switch command {
	case "build":
		values, positional, err := parseSandboxFlags(args[1:], map[string]flagKind{"ref": flagValue, "request-id": flagValue}, false)
		if err != nil || len(positional) != 0 || firstFlag(values, "ref") == "" || !validRequestID(firstFlag(values, "request-id")) {
			return a.developmentUsage(format, "dev build requires --ref COMMIT and --request-id ID")
		}
		if !exactCommit(firstFlag(values, "ref")) {
			return a.developmentUsage(format, "--ref must be an exact lowercase 40- or 64-hex commit")
		}
		commands, err := a.development()
		if err != nil {
			return a.writeDevelopmentError(format, err)
		}
		result, err := commands.Build(ctx, firstFlag(values, "ref"), firstFlag(values, "request-id"))
		return a.writeDevelopmentBuild(format, result, err)
	case "status":
		if len(args) != 2 || !uuidPatternCLI(args[1]) {
			return a.developmentUsage(format, "dev status requires BUILD")
		}
		commands, err := a.development()
		if err != nil {
			return a.writeDevelopmentError(format, err)
		}
		result, err := commands.Status(ctx, args[1])
		return a.writeDevelopmentBuild(format, result, err)
	default:
		return a.developmentUsage(format, fmt.Sprintf("unknown dev command %q", command))
	}
}

func (a *App) developmentUsage(format OutputFormat, message string) int {
	return a.writeError(format, ExitUsage, "usage", message)
}
func (a *App) writeDevelopmentBuild(format OutputFormat, result developmentpkg.BuildDocument, err error) int {
	if err != nil {
		return a.writeDevelopmentError(format, err)
	}
	if format == OutputJSON {
		return a.writeJSON(result)
	}
	id, status, version, _ := result.Summary()
	fmt.Fprintf(a.stdout, "Build %s [%s, version %d]\n", id, status, version)
	return ExitSuccess
}
func (a *App) writeDevelopmentError(format OutputFormat, err error) int {
	if errors.Is(err, workspacepkg.ErrNoContext) {
		return a.writeError(format, ExitUsage, "workspace_context_required", "select a Workspace and Project")
	}
	if errors.Is(err, projectpkg.ErrNoProject) || strings.Contains(err.Error(), "selections are required") {
		return a.writeError(format, ExitUsage, "project_context_required", "select a Project with 'blazn project use'")
	}
	if errors.Is(err, auth.ErrNotFound) {
		return a.writeError(format, ExitUnavailable, "not_authenticated", "run 'blazn auth login'")
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.Body.Code
		if code == "" {
			code = "api_error"
		}
		exit := ExitFailure
		if code == "access_expired" || code == "session_revoked" || code == "device_revoked" || code == "unauthorized" {
			exit = ExitUnavailable
		}
		return a.writeError(format, exit, code, apiErr.Error())
	}
	return a.writeError(format, ExitUnavailable, "development_unavailable", err.Error())
}
func exactCommit(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}
func uuidPatternCLI(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

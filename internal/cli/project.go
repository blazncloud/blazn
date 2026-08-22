package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/blazncloud/blazn/internal/auth"
	"github.com/blazncloud/blazn/internal/client"
	projectpkg "github.com/blazncloud/blazn/internal/project"
	workspacepkg "github.com/blazncloud/blazn/internal/workspace"
)

type projectCommands interface {
	Create(context.Context, string, string, string, string, string) (client.ProjectEnvelope, error)
	List(context.Context, string) (client.ProjectList, error)
	Get(context.Context, string) (client.ProjectEnvelope, error)
	Use(context.Context, string) (client.ProjectEnvelope, error)
	Update(context.Context, string, string, int, projectpkg.Update) (client.ProjectEnvelope, error)
	CurrentSelection(context.Context) (workspacepkg.Selection, error)
}

func (a *App) runProject(format OutputFormat, args []string) int {
	if len(args) == 0 || helpRequested(args) {
		return a.writeHelp(format, "project")
	}
	commands, err := a.project()
	if err != nil {
		return a.writeProjectError(format, err)
	}
	ctx := context.Background()
	switch args[0] {
	case "create":
		pos, flags, _, err := projectPositionalsAndFlags(args[1:], 1, map[string]bool{"slug": false, "kind": false, "description": true, "request-id": false})
		if err != nil || flags["request-id"] == "" {
			return a.projectUsage(format, errors.New("project create requires NAME and --request-id"))
		}
		result, err := commands.Create(ctx, pos[0], flags["slug"], flags["kind"], flags["description"], flags["request-id"])
		return a.writeProjectEnvelope(format, result, err, "created")
	case "list":
		pos, flags, _, err := projectPositionalsAndFlags(args[1:], 0, map[string]bool{"status": false})
		if err != nil || len(pos) != 0 {
			return a.projectUsage(format, errors.New("project list accepts only --status active|archived|all"))
		}
		status, err := projectpkg.ParseStatus(flags["status"])
		if err != nil {
			return a.projectUsage(format, err)
		}
		result, err := commands.List(ctx, status)
		return a.writeProjectList(format, result, err)
	case "get":
		if len(args) > 2 {
			return a.projectUsage(format, errors.New("project get accepts optional PROJECT"))
		}
		value := ""
		if len(args) == 2 {
			value = args[1]
		}
		result, err := commands.Get(ctx, value)
		return a.writeProjectEnvelope(format, result, err, "found")
	case "use":
		if len(args) != 2 {
			return a.projectUsage(format, errors.New("project use requires PROJECT"))
		}
		result, err := commands.Use(ctx, args[1])
		return a.writeProjectEnvelope(format, result, err, "selected")
	case "edit":
		pos, flags, present, err := projectPositionalsAndFlags(args[1:], 1, map[string]bool{"name": false, "description": true, "status": false, "expected-version": false, "request-id": false})
		if err != nil || flags["request-id"] == "" {
			return a.projectUsage(format, errors.New("project edit requires PROJECT, --expected-version, and --request-id"))
		}
		version, err := positiveVersion(flags["expected-version"])
		if err != nil {
			return a.projectUsage(format, err)
		}
		changes := projectpkg.Update{}
		if present["name"] {
			value := flags["name"]
			changes.Name = &value
		}
		if present["description"] {
			value := flags["description"]
			changes.Description = &value
		}
		if present["status"] {
			if flags["status"] != "active" && flags["status"] != "archived" {
				return a.projectUsage(format, errors.New("project status must be active or archived"))
			}
			value := client.ProjectStatus(flags["status"])
			changes.Status = &value
		}
		if changes.Name == nil && changes.Description == nil && changes.Status == nil {
			return a.projectUsage(format, errors.New("project edit requires --name, --description, or --status"))
		}
		result, err := commands.Update(ctx, pos[0], flags["request-id"], version, changes)
		return a.writeProjectEnvelope(format, result, err, "updated")
	default:
		return a.projectUsage(format, fmt.Errorf("unknown project command %q", args[0]))
	}
}

func projectPositionalsAndFlags(args []string, count int, allowed map[string]bool) ([]string, map[string]string, map[string]bool, error) {
	positionals := []string{}
	flags := map[string]string{}
	present := map[string]bool{}
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if !strings.HasPrefix(argument, "--") {
			positionals = append(positionals, argument)
			continue
		}
		name := strings.TrimPrefix(argument, "--")
		value := ""
		if split := strings.Index(name, "="); split >= 0 {
			value, name = name[split+1:], name[:split]
		} else {
			if index+1 >= len(args) {
				return nil, nil, nil, fmt.Errorf("--%s requires a value", name)
			}
			index++
			value = args[index]
		}
		allowEmpty, ok := allowed[name]
		if !ok || (!allowEmpty && value == "") || present[name] {
			return nil, nil, nil, fmt.Errorf("unknown, empty, or duplicate flag --%s", name)
		}
		flags[name], present[name] = value, true
	}
	if len(positionals) != count {
		return nil, nil, nil, fmt.Errorf("expected %d positional argument(s)", count)
	}
	return positionals, flags, present, nil
}

func (a *App) projectUsage(format OutputFormat, err error) int {
	return a.writeError(format, ExitUsage, "usage", err.Error())
}

func (a *App) writeProjectError(format OutputFormat, err error) int {
	if errors.Is(err, workspacepkg.ErrNoContext) {
		return a.writeError(format, ExitUsage, "workspace_context_required", "select a Workspace with 'blazn workspace use'")
	}
	if errors.Is(err, auth.ErrNotFound) {
		return a.writeError(format, ExitUnavailable, "not_authenticated", "run 'blazn auth login'")
	}
	if err.Error() == "no Project is selected" {
		return a.writeError(format, ExitUsage, "project_context_required", "select a Project with 'blazn project use'")
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
	return a.writeError(format, ExitUnavailable, "project_unavailable", err.Error())
}

func (a *App) writeProjectEnvelope(format OutputFormat, result client.ProjectEnvelope, err error, action string) int {
	if err != nil {
		return a.writeProjectError(format, err)
	}
	if format == OutputJSON {
		return a.writeJSON(result)
	}
	fmt.Fprintf(a.stdout, "%s Project %s (%s) [%s, version %d]\n", titleWord(action), result.Project.Name, result.Project.ID, result.Project.Status, result.Project.Version)
	return ExitSuccess
}

func (a *App) writeProjectList(format OutputFormat, result client.ProjectList, err error) int {
	if err != nil {
		return a.writeProjectError(format, err)
	}
	if result.Items == nil {
		result.Items = []client.Project{}
	}
	if format == OutputJSON {
		return a.writeJSON(result)
	}
	for _, project := range result.Items {
		fmt.Fprintf(a.stdout, "%-36s %-24s %-12s %-10s %s\n", project.ID, project.Slug, project.Kind, project.Status, project.Name)
	}
	return ExitSuccess
}

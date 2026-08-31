package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	agentharnesspkg "github.com/blazncloud/blazn/internal/agentharness"
	"github.com/blazncloud/blazn/internal/auth"
	"github.com/blazncloud/blazn/internal/client"
	workspacepkg "github.com/blazncloud/blazn/internal/workspace"
)

type agentHarnessCommands interface {
	CreateAgent(context.Context, string, []string, string) (client.AgentEnvelope, error)
	ListAgents(context.Context, string) (client.AgentList, error)
	GetAgent(context.Context, string) (client.AgentEnvelope, error)
	PublishAgent(context.Context, string, string, client.JSONDocument) (client.PublishAgentVersionEnvelope, error)
	ListAgentVersions(context.Context, string, string) (client.AgentVersionList, error)
	GetAgentVersion(context.Context, string, string) (client.AgentVersionEnvelope, error)
	CreateDefinition(context.Context, string, client.JSONDocument) (client.HarnessDefinitionEnvelope, error)
	ListDefinitions(context.Context, string) (client.HarnessDefinitionList, error)
	GetDefinition(context.Context, string) (client.HarnessDefinitionEnvelope, error)
	PublishVersion(context.Context, string, string, client.JSONDocument) (client.HarnessVersionEnvelope, error)
	ListVersions(context.Context, string, string) (client.HarnessVersionList, error)
	GetVersion(context.Context, string, string) (client.HarnessVersionEnvelope, error)
	CreateProfile(context.Context, string, client.JSONDocument) (client.HarnessProfileEnvelope, error)
	ListProfiles(context.Context, string) (client.HarnessProfileList, error)
	GetProfile(context.Context, string) (client.HarnessProfileEnvelope, error)
	ReviseProfile(context.Context, string, string, int, client.JSONDocument) (client.HarnessProfileEnvelope, error)
}

type agentHarnessInputError struct{ err error }

func (e agentHarnessInputError) Error() string { return e.err.Error() }

func fileDocument(args []string, positionals int, extra map[string]bool) ([]string, map[string]string, client.JSONDocument, error) {
	allowed := map[string]bool{"file": false}
	for k, v := range extra {
		allowed[k] = v
	}
	pos, flags, _, e := projectPositionalsAndFlags(args, positionals, allowed)
	if e != nil {
		return nil, nil, nil, agentHarnessInputError{e}
	}
	if flags["file"] == "" {
		return nil, nil, nil, agentHarnessInputError{errors.New("--file is required")}
	}
	d, e := agentharnesspkg.ReadDocument(flags["file"])
	return pos, flags, d, e
}
func commaValues(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(s, ",")
}

func (a *App) runAgent(format OutputFormat, args []string) int {
	if len(args) == 0 || helpRequested(args) {
		return a.writeHelp(format, "agent")
	}
	ctx := context.Background()
	if args[0] == "validate" {
		_, _, d, e := fileDocument(args[1:], 0, nil)
		if e == nil {
			e = agentharnesspkg.ValidateDocument("agent-version", d)
		}
		if e != nil {
			return a.writeAgentHarnessInputError(format, e)
		}
		if format == OutputJSON {
			return a.writeJSON(map[string]any{"valid": true, "document": d})
		}
		fmt.Fprintln(a.stdout, "AgentVersion document is valid")
		return ExitSuccess
	}
	c, e := a.agentHarness()
	if e != nil {
		return a.writeAgentHarnessError(format, e)
	}
	switch args[0] {
	case "create":
		pos, f, _, e := projectPositionalsAndFlags(args[1:], 1, map[string]bool{"tags": true, "request-id": false})
		if e != nil || f["request-id"] == "" {
			return a.ahUsage(format, "agent create requires NAME and --request-id")
		}
		r, e := c.CreateAgent(ctx, pos[0], commaValues(f["tags"]), f["request-id"])
		return a.ahOutput(format, r, e)
	case "list":
		_, f, _, e := projectPositionalsAndFlags(args[1:], 0, map[string]bool{"cursor": true})
		if e != nil {
			return a.ahUsage(format, "agent list accepts optional --cursor")
		}
		r, e := c.ListAgents(ctx, f["cursor"])
		return a.ahOutput(format, r, e)
	case "get":
		p, _, _, e := projectPositionalsAndFlags(args[1:], 1, map[string]bool{})
		if e != nil {
			return a.ahUsage(format, "agent get requires AGENT_ID")
		}
		r, e := c.GetAgent(ctx, p[0])
		return a.ahOutput(format, r, e)
	case "versions":
		p, f, _, e := projectPositionalsAndFlags(args[1:], 1, map[string]bool{"cursor": true})
		if e != nil {
			return a.ahUsage(format, "agent versions requires AGENT_ID")
		}
		r, e := c.ListAgentVersions(ctx, p[0], f["cursor"])
		return a.ahOutput(format, r, e)
	case "version":
		p, _, _, e := projectPositionalsAndFlags(args[1:], 2, map[string]bool{})
		if e != nil {
			return a.ahUsage(format, "agent version requires AGENT_ID VERSION_ID")
		}
		r, e := c.GetAgentVersion(ctx, p[0], p[1])
		return a.ahOutput(format, r, e)
	case "publish":
		p, f, d, e := fileDocument(args[1:], 1, map[string]bool{"request-id": false})
		if e != nil {
			return a.writeAgentHarnessInputError(format, e)
		}
		if f["request-id"] == "" {
			return a.ahUsage(format, "agent publish requires AGENT_ID --file FILE --request-id KEY")
		}
		if e = agentharnesspkg.ValidateDocument("agent-version", d); e != nil {
			return a.writeAgentHarnessInputError(format, e)
		}
		r, e := c.PublishAgent(ctx, p[0], f["request-id"], d)
		return a.ahOutput(format, r, e)
	default:
		return a.ahUsage(format, "unknown agent command")
	}
}

func (a *App) runHarness(format OutputFormat, args []string) int {
	if len(args) == 0 || helpRequested(args) {
		return a.writeHelp(format, "harness")
	}
	ctx := context.Background()
	if args[0] == "validate" {
		p, _, d, e := fileDocument(args[1:], 1, nil)
		if e == nil {
			e = agentharnesspkg.ValidateDocument(p[0], d)
		}
		if e != nil {
			return a.writeAgentHarnessInputError(format, e)
		}
		if format == OutputJSON {
			return a.writeJSON(map[string]any{"valid": true, "kind": p[0], "document": d})
		}
		fmt.Fprintf(a.stdout, "%s document is valid\n", p[0])
		return ExitSuccess
	}
	c, e := a.agentHarness()
	if e != nil {
		return a.writeAgentHarnessError(format, e)
	}
	switch args[0] {
	case "definitions":
		_, f, _, e := projectPositionalsAndFlags(args[1:], 0, map[string]bool{"cursor": true})
		if e != nil {
			return a.ahUsage(format, "harness definitions accepts optional --cursor")
		}
		r, e := c.ListDefinitions(ctx, f["cursor"])
		return a.ahOutput(format, r, e)
	case "definition":
		p, _, _, e := projectPositionalsAndFlags(args[1:], 1, map[string]bool{})
		if e != nil {
			return a.ahUsage(format, "harness definition requires DEFINITION_ID")
		}
		r, e := c.GetDefinition(ctx, p[0])
		return a.ahOutput(format, r, e)
	case "publish-definition":
		_, f, d, e := fileDocument(args[1:], 0, map[string]bool{"request-id": false})
		if e != nil {
			return a.writeAgentHarnessInputError(format, e)
		}
		if f["request-id"] == "" {
			return a.ahUsage(format, "harness publish-definition requires --file FILE --request-id KEY")
		}
		if e = agentharnesspkg.ValidateDocument("harness-definition", d); e != nil {
			return a.writeAgentHarnessInputError(format, e)
		}
		r, e := c.CreateDefinition(ctx, f["request-id"], d)
		return a.ahOutput(format, r, e)
	case "versions":
		p, f, _, e := projectPositionalsAndFlags(args[1:], 1, map[string]bool{"cursor": true})
		if e != nil {
			return a.ahUsage(format, "harness versions requires DEFINITION_ID")
		}
		r, e := c.ListVersions(ctx, p[0], f["cursor"])
		return a.ahOutput(format, r, e)
	case "version":
		p, _, _, e := projectPositionalsAndFlags(args[1:], 2, map[string]bool{})
		if e != nil {
			return a.ahUsage(format, "harness version requires DEFINITION_ID VERSION_ID")
		}
		r, e := c.GetVersion(ctx, p[0], p[1])
		return a.ahOutput(format, r, e)
	case "publish-version":
		p, f, d, e := fileDocument(args[1:], 1, map[string]bool{"request-id": false})
		if e != nil {
			return a.writeAgentHarnessInputError(format, e)
		}
		if f["request-id"] == "" {
			return a.ahUsage(format, "harness publish-version requires DEFINITION_ID --file FILE --request-id KEY")
		}
		if e = agentharnesspkg.ValidateDocument("harness-version", d); e != nil {
			return a.writeAgentHarnessInputError(format, e)
		}
		r, e := c.PublishVersion(ctx, p[0], f["request-id"], d)
		return a.ahOutput(format, r, e)
	case "profiles":
		_, f, _, e := projectPositionalsAndFlags(args[1:], 0, map[string]bool{"cursor": true})
		if e != nil {
			return a.ahUsage(format, "harness profiles accepts optional --cursor")
		}
		r, e := c.ListProfiles(ctx, f["cursor"])
		return a.ahOutput(format, r, e)
	case "profile":
		p, _, _, e := projectPositionalsAndFlags(args[1:], 1, map[string]bool{})
		if e != nil {
			return a.ahUsage(format, "harness profile requires PROFILE_ID")
		}
		r, e := c.GetProfile(ctx, p[0])
		return a.ahOutput(format, r, e)
	case "publish-profile":
		_, f, d, e := fileDocument(args[1:], 0, map[string]bool{"request-id": false})
		if e != nil {
			return a.writeAgentHarnessInputError(format, e)
		}
		if f["request-id"] == "" {
			return a.ahUsage(format, "harness publish-profile requires --file FILE --request-id KEY")
		}
		if e = agentharnesspkg.ValidateDocument("harness-profile", d); e != nil {
			return a.writeAgentHarnessInputError(format, e)
		}
		r, e := c.CreateProfile(ctx, f["request-id"], d)
		return a.ahOutput(format, r, e)
	case "revise-profile":
		p, f, d, e := fileDocument(args[1:], 1, map[string]bool{"request-id": false, "expected-version": false})
		if e != nil {
			return a.writeAgentHarnessInputError(format, e)
		}
		if f["request-id"] == "" {
			return a.ahUsage(format, "harness revise-profile requires PROFILE_ID --file FILE --expected-version N --request-id KEY")
		}
		n, e := strconv.Atoi(f["expected-version"])
		if e != nil || n < 1 {
			return a.ahUsage(format, "expected-version must be positive")
		}
		if e = agentharnesspkg.ValidateDocument("harness-profile", d); e != nil {
			return a.writeAgentHarnessInputError(format, e)
		}
		r, e := c.ReviseProfile(ctx, p[0], f["request-id"], n, d)
		return a.ahOutput(format, r, e)
	default:
		return a.ahUsage(format, "unknown harness command")
	}
}
func (a *App) ahUsage(f OutputFormat, m string) int { return a.writeError(f, ExitUsage, "usage", m) }
func (a *App) ahOutput(f OutputFormat, v any, e error) int {
	if e != nil {
		return a.writeAgentHarnessError(f, e)
	}
	if f == OutputJSON {
		return a.writeJSON(v)
	}
	data, _ := clientSummary(v)
	fmt.Fprintln(a.stdout, data)
	return ExitSuccess
}
func clientSummary(v any) (string, error) {
	switch x := v.(type) {
	case client.AgentEnvelope:
		return fmt.Sprintf("Agent %s (%s)", x.Agent.Name, x.Agent.ID), nil
	case client.AgentList:
		return fmt.Sprintf("%d Agent(s)", len(x.Items)), nil
	case client.PublishAgentVersionEnvelope:
		return fmt.Sprintf("Published AgentVersion %s", x.Version.ID), nil
	case client.AgentVersionList:
		return fmt.Sprintf("%d AgentVersion(s)", len(x.Items)), nil
	case client.AgentVersionEnvelope:
		return fmt.Sprintf("AgentVersion %s", x.Version.ID), nil
	case client.HarnessDefinitionEnvelope:
		return fmt.Sprintf("HarnessDefinition %s", x.Definition.ID), nil
	case client.HarnessDefinitionList:
		return fmt.Sprintf("%d HarnessDefinition(s)", len(x.Items)), nil
	case client.HarnessVersionEnvelope:
		return fmt.Sprintf("HarnessVersion %s", x.Version.ID), nil
	case client.HarnessVersionList:
		return fmt.Sprintf("%d HarnessVersion(s)", len(x.Items)), nil
	case client.HarnessProfileEnvelope:
		return fmt.Sprintf("HarnessProfile %s", x.Profile.ID), nil
	case client.HarnessProfileList:
		return fmt.Sprintf("%d HarnessProfile(s)", len(x.Items)), nil
	}
	return "OK", nil
}
func (a *App) writeAgentHarnessError(f OutputFormat, e error) int {
	if errors.Is(e, workspacepkg.ErrNoContext) {
		return a.writeError(f, ExitUsage, "workspace_context_required", "select a Workspace with 'blazn workspace use'")
	}
	if errors.Is(e, auth.ErrNotFound) {
		return a.writeError(f, ExitUnavailable, "not_authenticated", "run 'blazn auth login'")
	}
	var api *client.APIError
	if errors.As(e, &api) {
		return a.writeError(f, ExitFailure, api.Body.Code, api.Error())
	}
	return a.writeError(f, ExitUnavailable, "agent_harness_unavailable", e.Error())
}

func (a *App) writeAgentHarnessInputError(f OutputFormat, e error) int {
	var input agentHarnessInputError
	if errors.As(e, &input) {
		e = input.err
	}
	return a.writeError(f, ExitUsage, "invalid_document", e.Error())
}

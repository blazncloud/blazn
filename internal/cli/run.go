package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/blazncloud/blazn/internal/auth"
	"github.com/blazncloud/blazn/internal/client"
	projectpkg "github.com/blazncloud/blazn/internal/project"
	workspacepkg "github.com/blazncloud/blazn/internal/workspace"
)

type runCommands interface {
	ListMessages(context.Context, string, string) (client.RunMessageList, error)
	SendMessage(context.Context, string, string, client.SendRunMessageRequest) (client.RunMessageEnvelope, error)
	ClaimMessage(context.Context, string, string, int) (client.RunMessageClaimEnvelope, error)
	DeliverMessage(context.Context, string, string, string, string) (client.RunMessageEnvelope, error)
}

func (a *App) runRun(format OutputFormat, args []string) int {
	if len(args) == 0 || helpRequested(args) {
		return a.writeHelp(format, "run")
	}
	commands, err := a.run()
	if err != nil {
		return a.writeRunError(format, err)
	}
	ctx := context.Background()
	switch args[0] {
	case "messages":
		positionals, flags, _, err := projectPositionalsAndFlags(args[1:], 1, map[string]bool{"cursor": true})
		if err != nil {
			return a.runUsage(format, errors.New("run messages requires RUN and optional --cursor"))
		}
		result, err := commands.ListMessages(ctx, positionals[0], flags["cursor"])
		if err != nil {
			return a.writeRunError(format, err)
		}
		if format == OutputJSON {
			return a.writeJSON(result)
		}
		for _, message := range result.Items {
			fmt.Fprintf(a.stdout, "%d\t%s\t%s\t%s\n", message.Ordinal, message.Kind, message.Status, message.Content)
		}
		return ExitSuccess
	case "send":
		positionals, flags, _, err := projectPositionalsAndFlags(args[1:], 1, map[string]bool{"kind": false, "content": false, "parent": true, "request-id": false})
		if err != nil || flags["kind"] == "" || flags["content"] == "" || flags["request-id"] == "" {
			return a.runUsage(format, errors.New("run send requires RUN, --kind prompt|followup|steer, --content, and --request-id"))
		}
		kind := client.RunMessageKind(flags["kind"])
		if kind != client.RunMessageKindPrompt && kind != client.RunMessageKindFollowup && kind != client.RunMessageKindSteer {
			return a.runUsage(format, errors.New("run message kind must be prompt, followup, or steer"))
		}
		result, err := commands.SendMessage(ctx, positionals[0], flags["request-id"], client.SendRunMessageRequest{Kind: kind, Content: flags["content"], ParentMessageID: flags["parent"]})
		if err != nil {
			return a.writeRunError(format, err)
		}
		if format == OutputJSON {
			return a.writeJSON(result)
		}
		fmt.Fprintf(a.stdout, "queued %s message %s at ordinal %d\n", result.Message.Kind, result.Message.ID, result.Message.Ordinal)
		return ExitSuccess
	case "claim":
		positionals, flags, _, err := projectPositionalsAndFlags(args[1:], 1, map[string]bool{"lease": true, "request-id": false})
		if err != nil || flags["request-id"] == "" {
			return a.runUsage(format, errors.New("run claim requires RUN, --request-id, and optional --lease 5..300"))
		}
		lease := 30
		if flags["lease"] != "" {
			lease, err = strconv.Atoi(flags["lease"])
			if err != nil || lease < 5 || lease > 300 {
				return a.runUsage(format, errors.New("run claim --lease must be 5 through 300 seconds"))
			}
		}
		result, err := commands.ClaimMessage(ctx, positionals[0], flags["request-id"], lease)
		if err != nil {
			return a.writeRunError(format, err)
		}
		if format == OutputJSON {
			return a.writeJSON(result)
		}
		if result.Claim == nil {
			fmt.Fprintln(a.stdout, "no queued Run messages")
			return ExitSuccess
		}
		fmt.Fprintf(a.stdout, "claimed message %s until %s\n%s\n", result.Claim.Message.ID, result.Claim.LeaseExpiresAt, result.Claim.Message.Content)
		return ExitSuccess
	case "deliver":
		positionals, flags, _, err := projectPositionalsAndFlags(args[1:], 1, map[string]bool{"message": false, "claim": false, "request-id": false})
		if err != nil || flags["message"] == "" || flags["claim"] == "" || flags["request-id"] == "" {
			return a.runUsage(format, errors.New("run deliver requires RUN, --message, --claim, and --request-id"))
		}
		result, err := commands.DeliverMessage(ctx, positionals[0], flags["message"], flags["claim"], flags["request-id"])
		if err != nil {
			return a.writeRunError(format, err)
		}
		if format == OutputJSON {
			return a.writeJSON(result)
		}
		fmt.Fprintf(a.stdout, "delivered message %s at ordinal %d\n", result.Message.ID, result.Message.Ordinal)
		return ExitSuccess
	default:
		return a.runUsage(format, fmt.Errorf("unknown run command %q", args[0]))
	}
}

func (a *App) runUsage(format OutputFormat, err error) int {
	return a.writeError(format, ExitUsage, "usage", err.Error())
}

func (a *App) writeRunError(format OutputFormat, err error) int {
	if errors.Is(err, workspacepkg.ErrNoContext) {
		return a.writeError(format, ExitUsage, "workspace_context_required", "select a Workspace with 'blazn workspace use'")
	}
	if errors.Is(err, projectpkg.ErrNoProject) {
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
	return a.writeError(format, ExitUnavailable, "run_unavailable", err.Error())
}

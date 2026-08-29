package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

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
	RecordSyntheticProgress(context.Context, string, string, client.SyntheticRunProgressRequest) (client.ProgressAck, error)
	CompleteSynthetic(context.Context, string, string, client.CompleteSyntheticRunRequest) (client.RunEnvelope, error)
	Create(context.Context, string, client.CreateRunRequest) (client.RunEnvelope, error)
	List(context.Context, string, string) (client.RunList, error)
	Get(context.Context, string) (client.RunEnvelope, error)
	Cancel(context.Context, string, string, int) (client.RunEnvelope, error)
	Events(context.Context, string, string) (client.RunEventList, error)
	Progress(context.Context, string) (client.RunProgressList, error)
	Artifacts(context.Context, string, string) (client.ArtifactList, error)
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
	case "synthetic-progress":
		positionals, flags, _, err := projectPositionalsAndFlags(args[1:], 1, map[string]bool{"sequence": false, "phase": false, "percent": false, "message": true, "request-id": false})
		if err != nil || flags["sequence"] == "" || flags["phase"] == "" || flags["percent"] == "" || flags["request-id"] == "" {
			return a.runUsage(format, errors.New("run synthetic-progress requires RUN, --sequence, --phase, --percent, --request-id, and optional --message"))
		}
		sequence, sequenceErr := strconv.Atoi(flags["sequence"])
		percent, percentErr := strconv.Atoi(flags["percent"])
		if sequenceErr != nil || sequence < 0 || percentErr != nil || percent < 0 || percent > 100 {
			return a.runUsage(format, errors.New("run synthetic-progress requires a non-negative --sequence and --percent from 0 through 100"))
		}
		result, err := commands.RecordSyntheticProgress(ctx, positionals[0], flags["request-id"], client.SyntheticRunProgressRequest{Sequence: sequence, Phase: flags["phase"], Percent: percent, Message: flags["message"]})
		if err != nil {
			return a.writeRunError(format, err)
		}
		if format == OutputJSON {
			return a.writeJSON(result)
		}
		fmt.Fprintf(a.stdout, "run %s synthetic progress %d %s %d%%\n", result.RunID, result.Sequence, flags["phase"], percent)
		return ExitSuccess
	case "synthetic-complete":
		positionals, flags, _, err := projectPositionalsAndFlags(args[1:], 1, map[string]bool{"expected-version": false, "plan-digest": false, "artifacts": true, "steps": true, "warnings": true, "request-id": false})
		if err != nil || flags["expected-version"] == "" || flags["plan-digest"] == "" || flags["request-id"] == "" {
			return a.runUsage(format, errors.New("run synthetic-complete requires RUN, --expected-version, --plan-digest, --request-id, and optional comma-separated --artifacts/--warnings and --steps"))
		}
		expectedVersion, versionErr := strconv.Atoi(flags["expected-version"])
		steps := 0
		var stepsErr error
		if flags["steps"] != "" {
			steps, stepsErr = strconv.Atoi(flags["steps"])
		}
		if versionErr != nil || expectedVersion < 1 || stepsErr != nil || steps < 0 {
			return a.runUsage(format, errors.New("run synthetic-complete requires a positive --expected-version and non-negative --steps"))
		}
		result, err := commands.CompleteSynthetic(ctx, positionals[0], flags["request-id"], client.CompleteSyntheticRunRequest{ExpectedVersion: expectedVersion, PlanDigest: flags["plan-digest"], ArtifactIDs: commaList(flags["artifacts"]), Summary: client.RunReceiptSummary{Steps: steps, Warnings: commaList(flags["warnings"])}})
		if err != nil {
			return a.writeRunError(format, err)
		}
		if format == OutputJSON {
			return a.writeJSON(result)
		}
		fmt.Fprintf(a.stdout, "run %s %s synthetic receipt recorded\n", result.Run.ID, result.Run.Status)
		return ExitSuccess
	case "create":
		_, flags, _, err := projectPositionalsAndFlags(args[1:], 0, map[string]bool{"kind": false, "proof-class": false, "plan-digest": false, "inputs": true, "outputs": true, "request-id": false})
		if err != nil || flags["kind"] == "" || flags["proof-class"] == "" || flags["plan-digest"] == "" || flags["request-id"] == "" {
			return a.runUsage(format, errors.New("run create requires --kind, --proof-class, --plan-digest, --request-id, and optional comma-separated --inputs/--outputs"))
		}
		result, err := commands.Create(ctx, flags["request-id"], client.CreateRunRequest{Kind: flags["kind"], ProofClass: client.ProofClass(flags["proof-class"]), PlanDigest: flags["plan-digest"], InputArtifactIDs: commaList(flags["inputs"]), OutputNames: commaList(flags["outputs"])})
		if err != nil {
			return a.writeRunError(format, err)
		}
		if format == OutputJSON {
			return a.writeJSON(result)
		}
		fmt.Fprintf(a.stdout, "run %s %s\n", result.Run.ID, result.Run.Status)
		return ExitSuccess
	case "list":
		_, flags, _, err := projectPositionalsAndFlags(args[1:], 0, map[string]bool{"status": true, "cursor": true})
		if err != nil {
			return a.runUsage(format, errors.New("run list accepts optional --status and --cursor"))
		}
		result, err := commands.List(ctx, flags["status"], flags["cursor"])
		if err != nil {
			return a.writeRunError(format, err)
		}
		if format == OutputJSON {
			return a.writeJSON(result)
		}
		for _, run := range result.Items {
			fmt.Fprintf(a.stdout, "%s\t%s\t%s\t%s\n", run.ID, run.Kind, run.ProofClass, run.Status)
		}
		if result.NextCursor != nil {
			fmt.Fprintf(a.stdout, "next cursor: %s\n", *result.NextCursor)
		}
		return ExitSuccess
	case "get":
		positionals, _, _, err := projectPositionalsAndFlags(args[1:], 1, map[string]bool{})
		if err != nil {
			return a.runUsage(format, errors.New("run get requires RUN"))
		}
		result, err := commands.Get(ctx, positionals[0])
		if err != nil {
			return a.writeRunError(format, err)
		}
		if format == OutputJSON {
			return a.writeJSON(result)
		}
		fmt.Fprintf(a.stdout, "run %s %s version %d\n", result.Run.ID, result.Run.Status, result.Run.Version)
		return ExitSuccess
	case "cancel":
		positionals, flags, _, err := projectPositionalsAndFlags(args[1:], 1, map[string]bool{"expected-version": false, "request-id": false})
		if err != nil || flags["expected-version"] == "" || flags["request-id"] == "" {
			return a.runUsage(format, errors.New("run cancel requires RUN, --expected-version, and --request-id"))
		}
		expectedVersion, versionErr := strconv.Atoi(flags["expected-version"])
		if versionErr != nil || expectedVersion < 1 {
			return a.runUsage(format, errors.New("run cancel --expected-version must be a positive integer"))
		}
		result, err := commands.Cancel(ctx, positionals[0], flags["request-id"], expectedVersion)
		if err != nil {
			return a.writeRunError(format, err)
		}
		if format == OutputJSON {
			return a.writeJSON(result)
		}
		fmt.Fprintf(a.stdout, "run %s %s\n", result.Run.ID, result.Run.Status)
		return ExitSuccess
	case "watch":
		positionals, flags, _, err := projectPositionalsAndFlags(args[1:], 1, map[string]bool{"cursor": true, "interval-seconds": true})
		if err != nil {
			return a.runUsage(format, errors.New("run watch requires RUN and optional --cursor/--interval-seconds"))
		}
		interval := 2 * time.Second
		if flags["interval-seconds"] != "" {
			seconds, intervalErr := strconv.Atoi(flags["interval-seconds"])
			if intervalErr != nil || seconds < 1 || seconds > 60 {
				return a.runUsage(format, errors.New("run watch --interval-seconds must be 1 through 60"))
			}
			interval = time.Duration(seconds) * time.Second
		}
		cursor := flags["cursor"]
		failures := 0
		for {
			next, drainErr := a.drainRunEvents(ctx, format, commands, positionals[0], cursor)
			cursor = next
			if drainErr == nil {
				current, getErr := commands.Get(ctx, positionals[0])
				if getErr != nil {
					drainErr = getErr
				} else {
					failures = 0
					if current.Run.Status != client.RunStatusQueued && current.Run.Status != client.RunStatusRunning {
						if _, finalErr := a.drainRunEvents(ctx, format, commands, positionals[0], cursor); finalErr != nil {
							return a.writeRunError(format, finalErr)
						}
						if format == OutputJSON {
							encoded, encodeErr := json.Marshal(current)
							if encodeErr != nil {
								return a.writeError(format, ExitFailure, "encode_failed", encodeErr.Error())
							}
							fmt.Fprintln(a.stdout, string(encoded))
						} else {
							fmt.Fprintf(a.stdout, "run %s %s\n", current.Run.ID, current.Run.Status)
						}
						return ExitSuccess
					}
				}
			}
			if drainErr != nil {
				failures++
				if failures >= 3 {
					return a.writeRunError(format, drainErr)
				}
			}
			time.Sleep(interval)
		}
	case "logs":
		positionals, _, _, err := projectPositionalsAndFlags(args[1:], 1, map[string]bool{})
		if err != nil {
			return a.runUsage(format, errors.New("run logs requires RUN"))
		}
		result, err := commands.Progress(ctx, positionals[0])
		if err != nil {
			return a.writeRunError(format, err)
		}
		if format == OutputJSON {
			return a.writeJSON(result)
		}
		for _, entry := range result.Items {
			fmt.Fprintf(a.stdout, "%d\t%s\t%d%%\n", entry.Sequence, entry.Phase, entry.Percent)
		}
		return ExitSuccess
	case "result":
		positionals, _, _, err := projectPositionalsAndFlags(args[1:], 1, map[string]bool{})
		if err != nil {
			return a.runUsage(format, errors.New("run result requires RUN"))
		}
		result, err := commands.Get(ctx, positionals[0])
		if err != nil {
			return a.writeRunError(format, err)
		}
		if result.Run.Receipt == nil {
			return a.writeError(format, ExitFailure, "run_not_terminal", "Run has no terminal receipt yet")
		}
		if format == OutputJSON {
			return a.writeJSON(map[string]any{"runId": result.Run.ID, "status": result.Run.Status, "receipt": result.Run.Receipt})
		}
		fmt.Fprintf(a.stdout, "run %s %s outcome %s artifacts %d\n", result.Run.ID, result.Run.Status, result.Run.Receipt.Outcome, len(result.Run.Receipt.ArtifactIDs))
		return ExitSuccess
	case "artifacts":
		positionals, flags, _, err := projectPositionalsAndFlags(args[1:], 1, map[string]bool{"cursor": true})
		if err != nil {
			return a.runUsage(format, errors.New("run artifacts requires RUN and optional --cursor"))
		}
		result, err := commands.Artifacts(ctx, positionals[0], flags["cursor"])
		if err != nil {
			return a.writeRunError(format, err)
		}
		if format == OutputJSON {
			return a.writeJSON(result)
		}
		for _, artifact := range result.Items {
			fmt.Fprintf(a.stdout, "%s\t%s\t%s\t%s\n", artifact.ID, artifact.Name, artifact.Status, artifact.Digest)
		}
		if result.NextCursor != nil {
			fmt.Fprintf(a.stdout, "next cursor: %s\n", *result.NextCursor)
		}
		return ExitSuccess
	default:
		return a.runUsage(format, fmt.Errorf("unknown run command %q", args[0]))
	}
}

func (a *App) drainRunEvents(ctx context.Context, format OutputFormat, commands runCommands, runID, cursor string) (string, error) {
	for {
		events, err := commands.Events(ctx, runID, cursor)
		if err != nil {
			return cursor, err
		}
		for _, event := range events.Items {
			if format == OutputJSON {
				encoded, encodeErr := json.Marshal(event)
				if encodeErr != nil {
					return cursor, encodeErr
				}
				fmt.Fprintln(a.stdout, string(encoded))
			} else {
				fmt.Fprintf(a.stdout, "%d\t%s\n", event.Sequence, event.Type)
			}
			cursor = strconv.Itoa(event.Sequence)
		}
		if events.NextCursor == nil || len(events.Items) == 0 {
			return cursor, nil
		}
	}
}

func commaList(value string) []string {
	if value == "" {
		return []string{}
	}
	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			items = append(items, trimmed)
		}
	}
	return items
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

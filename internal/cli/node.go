package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/KingJammin/blazn/internal/client"
	nodepkg "github.com/KingJammin/blazn/internal/node"
)

type NodeEnrollOptions = nodepkg.CommandEnrollOptions
type nodeCommands interface {
	Enroll(context.Context, NodeEnrollOptions) (nodepkg.EnrollResult, error)
	Recover(context.Context) (client.NodeInstallReceipt, error)
	Heartbeat(context.Context) (nodepkg.HeartbeatResult, error)
}

func (a *App) runNode(format OutputFormat, args []string) int {
	if len(args) == 0 || helpRequested(args) {
		return a.writeHelp(format, "node")
	}
	commands, err := a.node()
	if err != nil {
		return a.writeError(format, ExitUnavailable, "node_unavailable", err.Error())
	}
	ctx := context.Background()
	switch args[0] {
	case "enroll":
		pos, flags, err := positionalAndFlags(args[1:], 0, map[string]bool{"workspace": true, "request-id": true, "name": true, "mode": true, "machine-fingerprint": true, "profile": true})
		if err != nil || len(pos) != 0 {
			return a.nodeUsage(format, err)
		}
		if flags["workspace"] == "" || len(flags["request-id"]) < 8 || flags["name"] == "" || flags["machine-fingerprint"] == "" || flags["profile"] == "" {
			return a.nodeUsage(format, errors.New("enroll requires --workspace, --request-id, --name, --mode, --machine-fingerprint, and --profile"))
		}
		mode := client.NodeEnrollmentMode(flags["mode"])
		if mode != client.NodeModeFresh && mode != client.NodeModeAdopt {
			return a.nodeUsage(format, errors.New("--mode must be fresh or adopt"))
		}
		result, err := commands.Enroll(ctx, NodeEnrollOptions{WorkspaceID: flags["workspace"], RequestID: flags["request-id"], Name: flags["name"], Mode: mode, MachineFingerprint: flags["machine-fingerprint"], ProfileFile: flags["profile"]})
		return a.writeNodeValue(format, result, err, "node enrolled and installed")
	case "recover":
		if len(args) != 1 {
			return a.nodeUsage(format, errors.New("recover accepts no arguments"))
		}
		result, err := commands.Recover(ctx)
		return a.writeNodeValue(format, result, err, "node install recovery completed")
	case "heartbeat":
		if len(args) != 1 {
			return a.nodeUsage(format, errors.New("heartbeat accepts no arguments"))
		}
		result, err := commands.Heartbeat(ctx)
		return a.writeNodeValue(format, result, err, "node heartbeat accepted")
	default:
		return a.writeError(format, ExitUsage, "unknown_command", fmt.Sprintf("unknown node command %q", args[0]))
	}
}

func (a *App) nodeUsage(format OutputFormat, err error) int {
	if err == nil {
		err = errors.New("invalid node command options")
	}
	return a.writeError(format, ExitUsage, "usage", err.Error())
}
func (a *App) writeNodeValue(format OutputFormat, value any, err error, message string) int {
	if err != nil {
		return a.writeError(format, ExitFailure, "node_failed", err.Error())
	}
	if format == OutputJSON {
		return a.writeJSON(value)
	}
	fmt.Fprintln(a.stdout, message)
	return ExitSuccess
}

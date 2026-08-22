package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/blazncloud/blazn/internal/client"
	nodepkg "github.com/blazncloud/blazn/internal/node"
	workspacepkg "github.com/blazncloud/blazn/internal/workspace"
)

type NodeEnrollOptions = nodepkg.CommandEnrollOptions
type nodeCommands interface {
	Enroll(context.Context, NodeEnrollOptions) (nodepkg.EnrollResult, error)
	Recover(context.Context) (client.NodeInstallReceipt, error)
	Heartbeat(context.Context) (nodepkg.HeartbeatResult, error)
	Serve(context.Context, time.Duration) error
}

func newDefaultNodeCommands(build BuildInfo) (nodeCommands, error) {
	sessions, err := workspacepkg.NewDefaultSessionProvider()
	if err != nil {
		return nil, err
	}
	session, err := sessions.Session(context.Background(), false)
	if err != nil {
		return nil, err
	}
	apiURL := os.Getenv("BLAZN_API_URL")
	if apiURL == "" {
		apiURL = sessions.Origin()
	}
	api, err := client.New(apiURL, &http.Client{Timeout: 30 * time.Second})
	if err != nil {
		return nil, err
	}
	return nodepkg.NewProductionCommandRuntime(api, session.AccessToken, build.Version, nil, nil, nodepkg.ProductionEmbeddedMaterials())
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
		pos, flags, err := positionalAndFlags(args[1:], 0, map[string]bool{"workspace": true, "request-id": true, "name": true, "mode": true, "machine-fingerprint": true, "profile": true, "cluster-id": true, "node-name": true, "node-uid": true, "resource-version": true})
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
		var binding *client.KubernetesBinding
		bindingValues := []string{flags["cluster-id"], flags["node-name"], flags["node-uid"], flags["resource-version"]}
		present := 0
		for _, value := range bindingValues {
			if value != "" {
				present++
			}
		}
		if mode == client.NodeModeAdopt {
			if present != len(bindingValues) || flags["node-name"] != flags["name"] {
				return a.nodeUsage(format, errors.New("adopt requires --cluster-id, --node-name matching --name, --node-uid, and --resource-version"))
			}
			binding = &client.KubernetesBinding{ClusterID: flags["cluster-id"], NodeName: flags["node-name"], NodeUID: flags["node-uid"], ResourceVersion: flags["resource-version"]}
		} else if present != 0 {
			return a.nodeUsage(format, errors.New("fresh enrollment does not accept an existing Kubernetes binding"))
		}
		result, err := commands.Enroll(ctx, NodeEnrollOptions{WorkspaceID: flags["workspace"], RequestID: flags["request-id"], Name: flags["name"], Mode: mode, MachineFingerprint: flags["machine-fingerprint"], ProfileFile: flags["profile"], KubernetesBinding: binding})
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
	case "serve":
		if len(args) != 1 || format != OutputHuman {
			return a.nodeUsage(format, errors.New("serve accepts no arguments and emits no structured output"))
		}
		if err := commands.Serve(ctx, 30*time.Second); err != nil && !errors.Is(err, context.Canceled) {
			return a.writeError(format, ExitFailure, "node_failed", err.Error())
		}
		return ExitSuccess
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

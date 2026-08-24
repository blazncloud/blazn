package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/blazncloud/blazn/internal/client"
	nodepkg "github.com/blazncloud/blazn/internal/node"
	workspacepkg "github.com/blazncloud/blazn/internal/workspace"
)

type NodeEnrollOptions = nodepkg.CommandEnrollOptions

var nodeUUIDPatternCLI = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type nodeCommands interface {
	Enroll(context.Context, NodeEnrollOptions) (nodepkg.EnrollResult, error)
	List(context.Context, string) (client.NodeList, error)
	Get(context.Context, string) (client.Node, error)
	Recover(context.Context) (client.NodeInstallReceipt, error)
	Repair(context.Context) (client.NodeInstallReceipt, error)
	Uninstall(context.Context, bool) (client.NodeInstallReceipt, error)
	Heartbeat(context.Context) (nodepkg.HeartbeatResult, error)
	Serve(context.Context, time.Duration) error
}

func newDefaultNodeCommands(build BuildInfo, daemonOnly bool) (nodeCommands, error) {
	if daemonOnly {
		return nodepkg.NewProductionDaemonCommandRuntime(build.Version, &http.Client{Timeout: 30 * time.Second}, nil)
	}
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
	commands, err := a.node(args[0] == "serve" || args[0] == "heartbeat")
	if err != nil {
		return a.writeError(format, ExitUnavailable, "node_unavailable", err.Error())
	}
	ctx := context.Background()
	switch args[0] {
	case "list":
		pos, flags, err := positionalAndFlags(args[1:], 0, map[string]bool{"workspace": true})
		if err != nil || len(pos) != 0 || flags["workspace"] == "" {
			return a.nodeUsage(format, errors.New("node list requires --workspace WORKSPACE"))
		}
		result, err := commands.List(ctx, flags["workspace"])
		if err != nil {
			return a.writeError(format, ExitFailure, "node_failed", err.Error())
		}
		if format == OutputJSON {
			return a.writeJSON(result)
		}
		for _, node := range result.Items {
			fmt.Fprintf(a.stdout, "%s\t%s\t%s\teligible=%t\tcapability=%s\n", node.ID, node.Name, node.LifecycleState, node.AgentEligible, capabilityVersion(node.CapabilityVersion))
		}
		return ExitSuccess
	case "get", "capacity":
		if len(args) != 2 || !nodeUUIDPatternCLI.MatchString(args[1]) {
			return a.nodeUsage(format, fmt.Errorf("node %s requires NODE", args[0]))
		}
		result, err := commands.Get(ctx, args[1])
		if err != nil {
			return a.writeError(format, ExitFailure, "node_failed", err.Error())
		}
		if args[0] == "capacity" {
			view := nodeCapacityView{NodeID: result.ID, Name: result.Name, LifecycleState: result.LifecycleState, TrustState: result.TrustState, AgentEligible: result.AgentEligible, CapabilityVersion: result.CapabilityVersion, KubernetesBinding: result.KubernetesBinding}
			if format == OutputJSON {
				return a.writeJSON(view)
			}
			fmt.Fprintf(a.stdout, "Node %s capacity: eligible=%t, lifecycle=%s, trust=%s, capability=%s\n", result.ID, result.AgentEligible, result.LifecycleState, result.TrustState, capabilityVersion(result.CapabilityVersion))
			return ExitSuccess
		}
		if format == OutputJSON {
			return a.writeJSON(result)
		}
		fmt.Fprintf(a.stdout, "Node %s (%s) [%s, trust=%s, eligible=%t, capability=%s]\n", result.ID, result.Name, result.LifecycleState, result.TrustState, result.AgentEligible, capabilityVersion(result.CapabilityVersion))
		return ExitSuccess
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
	case "repair":
		if len(args) != 1 {
			return a.nodeUsage(format, errors.New("repair accepts no arguments"))
		}
		result, err := commands.Repair(ctx)
		return a.writeNodeValue(format, result, err, "node repair completed")
	case "uninstall":
		confirmed, removeManaged := false, false
		for _, arg := range args[1:] {
			if arg == "--yes" {
				confirmed = true
			} else if arg == "--remove-managed-runtime" {
				removeManaged = true
			} else {
				return a.nodeUsage(format, fmt.Errorf("unknown node uninstall option %q", arg))
			}
		}
		if !confirmed {
			return a.nodeUsage(format, errors.New("node uninstall requires --yes"))
		}
		result, err := commands.Uninstall(ctx, removeManaged)
		return a.writeNodeValue(format, result, err, "node uninstall completed")
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

type nodeCapacityView struct {
	NodeID            string                    `json:"nodeId"`
	Name              string                    `json:"name"`
	LifecycleState    client.NodeLifecycleState `json:"lifecycleState"`
	TrustState        client.NodeTrustState     `json:"trustState"`
	AgentEligible     bool                      `json:"agentEligible"`
	CapabilityVersion *int64                    `json:"capabilityVersion"`
	KubernetesBinding *client.KubernetesBinding `json:"kubernetesBinding,omitempty"`
}

func capabilityVersion(value *int64) string {
	if value == nil {
		return "none"
	}
	return fmt.Sprint(*value)
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

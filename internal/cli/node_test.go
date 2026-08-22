package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/client"
	nodepkg "github.com/blazncloud/blazn/internal/node"
)

type fakeNodeCommands struct {
	options          NodeEnrollOptions
	heartbeats       int
	repairs          int
	uninstallManaged bool
}

func (f *fakeNodeCommands) Serve(context.Context, time.Duration) error { return nil }

func (f *fakeNodeCommands) Enroll(_ context.Context, options NodeEnrollOptions) (nodepkg.EnrollResult, error) {
	f.options = options
	return nodepkg.EnrollResult{Installed: true}, nil
}
func (*fakeNodeCommands) Recover(context.Context) (client.NodeInstallReceipt, error) {
	return client.NodeInstallReceipt{State: "removed"}, nil
}
func (f *fakeNodeCommands) Repair(context.Context) (client.NodeInstallReceipt, error) {
	f.repairs++
	return client.NodeInstallReceipt{State: "active"}, nil
}
func (f *fakeNodeCommands) Uninstall(_ context.Context, managed bool) (client.NodeInstallReceipt, error) {
	f.uninstallManaged = managed
	return client.NodeInstallReceipt{State: "removed"}, nil
}
func (f *fakeNodeCommands) Heartbeat(context.Context) (nodepkg.HeartbeatResult, error) {
	f.heartbeats++
	return nodepkg.HeartbeatResult{NodeID: "node-a", Sequence: 0}, nil
}

func TestNodeCLIRequiresExplicitSafeEnrollmentInputsAndJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	fake := &fakeNodeCommands{}
	app := New(&stdout, &stderr, BuildInfo{})
	app.node = func(bool) (nodeCommands, error) { return fake, nil }
	code := app.Run([]string{"--output", "json", "node", "enroll", "--workspace", "workspace-a", "--request-id", "request-1", "--name", "node-a", "--mode", "fresh", "--machine-fingerprint", strings.Repeat("a", 64), "--profile", "/etc/blazn/profile.json"})
	if code != ExitSuccess {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if fake.options.WorkspaceID != "workspace-a" || fake.options.Mode != client.NodeModeFresh {
		t.Fatalf("options=%#v", fake.options)
	}
	var result nodepkg.EnrollResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || !result.Installed {
		t.Fatalf("json=%s err=%v", stdout.String(), err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"node", "enroll", "--workspace", "workspace-a"}); code != ExitUsage {
		t.Fatalf("unsafe code=%d", code)
	}
}

func TestNodeHelpAndHeartbeat(t *testing.T) {
	var stdout, stderr bytes.Buffer
	fake := &fakeNodeCommands{}
	app := New(&stdout, &stderr, BuildInfo{})
	app.node = func(bool) (nodeCommands, error) { return fake, nil }
	if code := app.Run([]string{"help", "node"}); code != 0 || !strings.Contains(stdout.String(), "node enroll|recover|repair|uninstall|heartbeat|serve") || !strings.Contains(stdout.String(), "root-authorize") {
		t.Fatalf("help=%q code=%d", stdout.String(), code)
	}
	stdout.Reset()
	if code := app.Run([]string{"node", "heartbeat"}); code != 0 || fake.heartbeats != 1 {
		t.Fatalf("code=%d calls=%d", code, fake.heartbeats)
	}
}

func TestNodeCLIRequiresAndCarriesExactAdoptedBinding(t *testing.T) {
	var stdout, stderr bytes.Buffer
	fake := &fakeNodeCommands{}
	app := New(&stdout, &stderr, BuildInfo{})
	app.node = func(bool) (nodeCommands, error) { return fake, nil }
	base := []string{"node", "enroll", "--workspace", "workspace-a", "--request-id", "request-1", "--name", "node-a", "--mode", "adopt", "--machine-fingerprint", strings.Repeat("a", 64), "--profile", "/etc/blazn/profile.json"}
	if code := app.Run(base); code != ExitUsage {
		t.Fatalf("adopt without binding code=%d", code)
	}
	args := append(base, "--cluster-id", "cluster-a", "--node-name", "node-a", "--node-uid", "uid-a", "--resource-version", "17")
	stdout.Reset()
	stderr.Reset()
	if code := app.Run(args); code != ExitSuccess {
		t.Fatalf("adopt code=%d stderr=%s", code, stderr.String())
	}
	want := client.KubernetesBinding{ClusterID: "cluster-a", NodeName: "node-a", NodeUID: "uid-a", ResourceVersion: "17"}
	if fake.options.KubernetesBinding == nil || *fake.options.KubernetesBinding != want {
		t.Fatalf("binding=%#v", fake.options.KubernetesBinding)
	}
}

func TestNodeRepairAndConfirmedUninstallCLI(t *testing.T) {
	var stdout, stderr bytes.Buffer
	fake := &fakeNodeCommands{}
	app := New(&stdout, &stderr, BuildInfo{})
	app.node = func(bool) (nodeCommands, error) { return fake, nil }
	if code := app.Run([]string{"node", "repair"}); code != ExitSuccess || fake.repairs != 1 {
		t.Fatalf("repair code=%d calls=%d", code, fake.repairs)
	}
	if code := app.Run([]string{"node", "uninstall"}); code != ExitUsage {
		t.Fatalf("unconfirmed uninstall code=%d", code)
	}
	if code := app.Run([]string{"node", "uninstall", "--yes", "--remove-managed-runtime"}); code != ExitSuccess || !fake.uninstallManaged {
		t.Fatalf("uninstall code=%d managed=%v stderr=%s", code, fake.uninstallManaged, stderr.String())
	}
}

func TestNewAppWiresDefaultNodeCommandFactory(t *testing.T) {
	prior := defaultNodeCommandFactory
	defer func() { defaultNodeCommandFactory = prior }()
	fake := &fakeNodeCommands{}
	called := false
	defaultNodeCommandFactory = func(build BuildInfo, daemonOnly bool) (nodeCommands, error) {
		called = true
		if !daemonOnly {
			t.Fatal("heartbeat requested interactive node construction")
		}
		if build.Version != "v-test" {
			t.Fatalf("build=%#v", build)
		}
		return fake, nil
	}
	var stdout, stderr bytes.Buffer
	app := New(&stdout, &stderr, BuildInfo{Version: "v-test"})
	if code := app.Run([]string{"node", "heartbeat"}); code != ExitSuccess || !called || fake.heartbeats != 1 {
		t.Fatalf("code=%d called=%v heartbeats=%d stderr=%s", code, called, fake.heartbeats, stderr.String())
	}
}

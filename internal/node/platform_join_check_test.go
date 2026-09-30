package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/client"
)

func TestJoinCertificateCheckMatchesMicroK8s(t *testing.T) {
	for _, value := range []string{"fee24a55f47e", "check-value-123456"} {
		if !validJoinCertificateCheck(value) {
			t.Fatalf("%q rejected", value)
		}
	}
	for _, value := range []string{"", "short", "fee24a55f47", "fee24a55 f47e", "fee24a55f47e/x"} {
		if validJoinCertificateCheck(value) {
			t.Fatalf("%q accepted", value)
		}
	}
}

func TestPreferredJoinURLMatchesSignedAPIServerHost(t *testing.T) {
	check := "0123456789abcdef0123456789abcdef/fee24a55f47e"
	urls := []string{"10.90.0.10:25000/" + check, "100.92.226.60:25000/" + check, "192.168.0.100:25000/" + check, "192.168.0.108:25000/" + check}
	if got := preferredJoinURL(urls, "https://192.168.0.108:16443"); got != "192.168.0.108:25000/"+check {
		t.Fatalf("selected %q", got)
	}
	if got := preferredJoinURL(urls, "https://192.168.0.200:16443"); got != urls[0] {
		t.Fatalf("fallback selected %q", got)
	}
}

func TestMicroK8sJoinRunsInSnapContextWithTokenOnStdinOnly(t *testing.T) {
	args := microK8sJoinArguments()
	if len(args) != 7 || args[0] != "run" || args[1] != "--shell" || args[2] != "microk8s" || args[3] != "-c" || args[6] != microK8sJoinStdinProgram {
		t.Fatalf("unexpected join invocation %#v", args)
	}
	for _, arg := range args {
		if strings.Contains(arg, ":25000/") {
			t.Fatalf("join URL leaked into argv: %q", arg)
		}
	}
	if !strings.Contains(microK8sJoinStdinProgram, `sys.path.insert(0,"/snap/microk8s/current/scripts/wrappers")`) || !strings.Contains(microK8sJoinStdinProgram, "sys.stdin.readline()") {
		t.Fatal("join program must import MicroK8s wrapper modules and read the URL from stdin")
	}
}

func TestJoinRegistersTheWorkerWithTheBootstrapTaint(t *testing.T) {
	// MicroK8s worker join replaces the kubelet args file with the control
	// plane's arguments, so the taint must be appended inside that step.
	for _, fragment := range []string{"m.store_base_kubelet_args=w", `t="` + microK8sBootstrapTaintArgument + `"`, `"--register-with-taints" in a`} {
		if !strings.Contains(microK8sJoinStdinProgram, fragment) {
			t.Fatalf("join program lacks %q", fragment)
		}
	}
	args := nodeKubectlArguments([]string{"get", "node", "worker-a"})
	if len(args) != 5 || args[0] != "--kubeconfig" || args[1] != microK8sKubeletKubeconfig || args[2] != "get" {
		t.Fatalf("node kubectl does not use the kubelet node credential: %#v", args)
	}
}

func TestObserveNodeWaitsForTheRestartedWorkerProxy(t *testing.T) {
	previousAttempts, previousInterval := joinObservationAttempts, joinObservationInterval
	joinObservationAttempts, joinObservationInterval = 5, time.Millisecond
	defer func() { joinObservationAttempts, joinObservationInterval = previousAttempts, previousInterval }()
	calls := 0
	engine := NativeRootEngine{Platform: "linux", Commands: scriptedExecutor{run: func(path string, args []string, _ []byte) ([]byte, error) {
		calls++
		if calls < 3 {
			return nil, errors.New("connection refused")
		}
		return []byte(`{"metadata":{"name":"worker-1","uid":"uid-1","resourceVersion":"7"}}`), nil
	}}}
	node, err := engine.observeNode(context.Background(), testJoinPlan("linux"), "worker-1")
	if err != nil || node.UID != "uid-1" || calls != 3 {
		t.Fatalf("node=%#v err=%v calls=%d", node, err, calls)
	}
	calls = -100
	if _, err := engine.observeNode(context.Background(), testJoinPlan("linux"), "worker-1"); err == nil || calls != -95 {
		t.Fatalf("unbounded retry: err=%v calls=%d", err, calls)
	}
}

func TestFreshSnapInstallVerifiesTheLocalRevisionByDigest(t *testing.T) {
	directory := t.TempDir()
	previous := snapdSnapsDirectory
	snapdSnapsDirectory = directory
	defer func() { snapdSnapsDirectory = previous }()
	content := []byte("pinned microk8s snap")
	sum := sha256.Sum256(content)
	if err := os.WriteFile(filepath.Join(directory, "microk8s_x1.snap"), content, 0600); err != nil {
		t.Fatal(err)
	}
	engine := NativeRootEngine{Platform: "linux", Commands: scriptedExecutor{run: func(path string, args []string, _ []byte) ([]byte, error) {
		return []byte("Name      Version  Rev  Tracking  Publisher  Notes\nmicrok8s  v1.35.6  x1   -         -          classic\n"), nil
	}}}
	mutation := client.NodeInstallMutation{Kind: "package", Action: "install", Target: "microk8s", Desired: map[string]any{"manager": "snap", "version": "v1.35.6-rev9072"}, DesiredDigest: "sha256:" + hex.EncodeToString(sum[:])}
	if err := engine.verifyPackage(context.Background(), mutation, "snap"); err != nil {
		t.Fatalf("digest-identical local snap rejected: %v", err)
	}
	wrong := mutation
	wrong.DesiredDigest = "sha256:" + strings.Repeat("0", 64)
	if err := engine.verifyPackage(context.Background(), wrong, "snap"); err == nil {
		t.Fatal("local snap with a different digest accepted")
	}
	adopt := mutation
	adopt.Action = "adopt_exact"
	if err := engine.verifyPackage(context.Background(), adopt, "snap"); err == nil {
		t.Fatal("adoption accepted a local revision instead of the exact store revision")
	}
}

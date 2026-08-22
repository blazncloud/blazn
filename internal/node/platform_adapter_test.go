package node

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/KingJammin/blazn/internal/client"
)

type recordedCommand struct {
	path  string
	args  []string
	input []byte
}

type recordingExecutor struct {
	calls []recordedCommand
}

func (e *recordingExecutor) Run(ctx context.Context, path string, args ...string) ([]byte, error) {
	return e.run(path, nil, args...)
}

func (e *recordingExecutor) RunInput(ctx context.Context, path string, input []byte, args ...string) ([]byte, error) {
	return e.run(path, input, args...)
}

func (e *recordingExecutor) run(path string, input []byte, args ...string) ([]byte, error) {
	e.calls = append(e.calls, recordedCommand{path: path, args: append([]string(nil), args...), input: append([]byte(nil), input...)})
	if strings.HasSuffix(path, "microk8s.kubectl") || (path == "/usr/local/bin/limactl" && containsArgument(args, "/snap/bin/microk8s.kubectl")) {
		return []byte(`{"metadata":{"name":"worker-1","uid":"uid-1","resourceVersion":"7"},"spec":{"taints":[{"key":"blazn.dev/bootstrap","value":"pending","effect":"NoSchedule"}]}}`), nil
	}
	return nil, nil
}

func containsArgument(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func testJoinPlan(platform string) client.NodeInstallPlan {
	targetPlatform := client.NodePlatformLinux
	architecture := client.NodeArchAMD64
	if platform == "macos" {
		targetPlatform = client.NodePlatformMacOS
		architecture = client.NodeArchARM64
	}
	return client.NodeInstallPlan{
		Hostname: "worker-1",
		Cluster: client.NodeInstallCluster{
			ID:             "cluster-1",
			WorkerOnly:     true,
			BootstrapTaint: "blazn.dev/bootstrap=pending:NoSchedule",
		},
		Target: client.NodeInstallTarget{Platform: targetPlatform, Architecture: architecture},
	}
}

func encodedJoinCredential(t *testing.T) (string, string) {
	t.Helper()
	endpoint := "192.0.2.10:25000/" + strings.Repeat("a", 32) + "/" + strings.Repeat("b", 32)
	value, err := json.Marshal(joinPayload{
		SchemaVersion:    "blazn.dev/microk8s-worker-join/v1",
		IssuanceID:       "issuance-1",
		ClusterID:        "cluster-1",
		ExpectedNodeName: "worker-1",
		BootstrapTaint:   "blazn.dev/bootstrap=pending:NoSchedule",
		WorkerOnly:       true,
		ExpiresAt:        time.Now().Add(time.Minute),
		URLs:             []string{endpoint},
	})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(value), endpoint
}

func TestLinuxJoinKeepsCredentialOutOfArguments(t *testing.T) {
	credential, endpoint := encodedJoinCredential(t)
	commands := &recordingExecutor{}
	engine := NativeRootEngine{Platform: "linux", Commands: commands, allowTestJoinRuntime: true}
	binding := &RootJoinBinding{Credential: credential, ClusterID: "cluster-1", ExpectedNodeName: "worker-1", BootstrapTaint: "blazn.dev/bootstrap=pending:NoSchedule", WorkerOnly: true}
	joined, err := engine.join(context.Background(), testJoinPlan("linux"), binding)
	if err != nil {
		t.Fatal(err)
	}
	if joined.Name != "worker-1" || joined.UID != "uid-1" || joined.ResourceVersion != "7" {
		t.Fatalf("unexpected joined binding: %+v", joined)
	}
	if len(commands.calls) < 2 || string(commands.calls[0].input) != endpoint+"\n" {
		t.Fatalf("join credential was not supplied over stdin: %+v", commands.calls)
	}
	for _, call := range commands.calls {
		if strings.Contains(strings.Join(call.args, " "), endpoint) {
			t.Fatal("join credential leaked into command arguments")
		}
	}
}

func TestMacJoinUsesDigestBoundLimaVMAndStdin(t *testing.T) {
	credential, endpoint := encodedJoinCredential(t)
	bindingValue := []byte(`{"schemaVersion":"blazn.dev/lima-worker-binding/v1","clusterId":"cluster-1","vmName":"blazn-worker-fixed","workerName":"worker-1"}`)
	sum := sha256.Sum256(bindingValue)
	path := filepath.Join(t.TempDir(), "lima-binding.json")
	if err := os.WriteFile(path, bindingValue, 0600); err != nil {
		t.Fatal(err)
	}
	plan := testJoinPlan("macos")
	plan.Components = []client.NodeInstallComponent{{Name: "lima-worker-binding", ArtifactType: "configuration", SourceClass: "embedded", SHA256: hex.EncodeToString(sum[:])}}
	commands := &recordingExecutor{}
	engine := NativeRootEngine{Platform: "macos", Commands: commands, LimaBindingPath: path, allowTestJoinRuntime: true}
	binding := &RootJoinBinding{Credential: credential, ClusterID: "cluster-1", ExpectedNodeName: "worker-1", BootstrapTaint: "blazn.dev/bootstrap=pending:NoSchedule", WorkerOnly: true}
	if _, err := engine.join(context.Background(), plan, binding); err != nil {
		t.Fatal(err)
	}
	if len(commands.calls) < 2 || commands.calls[0].path != "/usr/local/bin/limactl" || !containsArgument(commands.calls[0].args, "blazn-worker-fixed") || string(commands.calls[0].input) != endpoint+"\n" {
		t.Fatalf("join did not use the digest-bound Lima VM and stdin: %+v", commands.calls)
	}
	for _, call := range commands.calls {
		if strings.Contains(strings.Join(call.args, " "), endpoint) {
			t.Fatal("join credential leaked into Lima arguments")
		}
	}
}

func TestReadLimaVMRejectsDigestAndBindingMismatch(t *testing.T) {
	value := []byte(`{"schemaVersion":"blazn.dev/lima-worker-binding/v1","clusterId":"cluster-1","vmName":"fixed","workerName":"worker-1"}`)
	path := filepath.Join(t.TempDir(), "binding.json")
	if err := os.WriteFile(path, value, 0600); err != nil {
		t.Fatal(err)
	}
	plan := testJoinPlan("macos")
	plan.Components = []client.NodeInstallComponent{{Name: "lima-worker-binding", ArtifactType: "configuration", SourceClass: "embedded", SHA256: strings.Repeat("0", 64)}}
	if _, err := readLimaVM(plan, path); err == nil {
		t.Fatal("mismatched Lima binding digest was accepted")
	}
	sum := sha256.Sum256(value)
	plan.Components[0].SHA256 = hex.EncodeToString(sum[:])
	plan.Hostname = "other-worker"
	if _, err := readLimaVM(plan, path); err == nil {
		t.Fatal("mismatched Lima worker binding was accepted")
	}
}

func TestRollbackAbsentDirectoryNeverBroadDeletes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix ownership semantics")
	}
	directory := filepath.Join(t.TempDir(), "owned")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(directory, "unmanaged")
	if err := os.WriteFile(marker, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	engine := NativeRootEngine{Platform: "linux", Commands: &recordingExecutor{}}
	mutation := client.NodeInstallMutation{Kind: "directory", Target: directory}
	err := engine.rollback(context.Background(), client.NodeInstallPlan{}, mutation, PriorState{State: "absent", Material: client.NodeRollbackMaterial{Kind: "absent"}}, t.TempDir(), nil)
	if err == nil {
		t.Fatal("non-empty directory rollback unexpectedly succeeded")
	}
	if value, readErr := os.ReadFile(marker); readErr != nil || string(value) != "preserve" {
		t.Fatal("rollback removed unmanaged content")
	}
}

func TestValidateRootRequestMaterialBindsSignedComponent(t *testing.T) {
	plan := client.NodeInstallPlan{
		Components: []client.NodeInstallComponent{{Name: "service", SHA256: strings.Repeat("a", 64)}},
		Mutations:  []client.NodeInstallMutation{{Ordinal: 1, Kind: "file", Desired: map[string]any{"sourceComponent": "service"}}},
	}
	request := RootRequest{Operation: RootApply, Plan: plan, Ordinal: 1, Material: &RootMaterial{ComponentName: "service", SHA256: strings.Repeat("b", 64)}}
	if err := validateRootRequestMaterial(request); err == nil {
		t.Fatal("root helper accepted material with an unbound digest")
	}
	request.Material.SHA256 = strings.Repeat("a", 64)
	if err := validateRootRequestMaterial(request); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeJoinCredentialRejectsUnknownFieldsAndCredentials(t *testing.T) {
	binding := &RootJoinBinding{ClusterID: "cluster-1", ExpectedNodeName: "worker-1", BootstrapTaint: "blazn.dev/bootstrap=pending:NoSchedule", WorkerOnly: true}
	credential, _ := encodedJoinCredential(t)
	if _, _, err := decodeJoinCredential(credential, binding); err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(credential)
	var value map[string]any
	_ = json.Unmarshal(raw, &value)
	value["unexpected"] = true
	raw, _ = json.Marshal(value)
	if _, _, err := decodeJoinCredential(base64.RawURLEncoding.EncodeToString(raw), binding); err == nil {
		t.Fatal("unknown credential field was accepted")
	}
	value["urls"] = []string{"user:secret@192.0.2.10:25000/" + strings.Repeat("a", 32) + "/" + strings.Repeat("b", 32)}
	delete(value, "unexpected")
	raw, _ = json.Marshal(value)
	if _, _, err := decodeJoinCredential(base64.RawURLEncoding.EncodeToString(raw), binding); err == nil {
		t.Fatal("credential URL userinfo was accepted")
	}
}

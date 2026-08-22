package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
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

type mockJoinAPI struct {
	issueRequest   client.JoinCredentialRequest
	issueProof     string
	consumeRequest client.ConsumeJoinCredentialRequest
	consumeProof   string
	credential     client.JoinCredential
	node           client.Node
}

func (m *mockJoinAPI) IssueNodeJoinCredential(_ context.Context, proof, _ string, request client.JoinCredentialRequest) (client.JoinCredential, error) {
	m.issueProof, m.issueRequest = proof, request
	return m.credential, nil
}

func (m *mockJoinAPI) ConsumeNodeJoinCredential(_ context.Context, proof, _ string, _ string, request client.ConsumeJoinCredentialRequest) (client.Node, error) {
	m.consumeProof, m.consumeRequest = proof, request
	return m.node, nil
}

type recordedCommand struct {
	path  string
	args  []string
	input []byte
}

type recordingExecutor struct {
	calls []recordedCommand
}

type scriptedExecutor struct {
	run func(string, []string, []byte) ([]byte, error)
}

func (e scriptedExecutor) Run(_ context.Context, path string, args ...string) ([]byte, error) {
	return e.run(path, args, nil)
}

func (e scriptedExecutor) RunInput(_ context.Context, path string, input []byte, args ...string) ([]byte, error) {
	return e.run(path, args, input)
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

func TestBrokerJoinCoordinatorBindsIssueAndConsumeToPersistedIdentity(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	identity := Identity{PrivateKey: privateKey, PublicKey: privateKey.Public().(ed25519.PublicKey)}
	fingerprint, err := identity.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	plan := testJoinPlan("linux")
	plan.PlanID = "11111111-1111-4111-8111-111111111111"
	plan.NodeID = "22222222-2222-4222-8222-222222222222"
	plan.EnrollmentID = "33333333-3333-4333-8333-333333333333"
	plan.WorkspaceID = "44444444-4444-4444-8444-444444444444"
	plan.Digest = "sha256:" + strings.Repeat("a", 64)
	plan.Target.MachineFingerprint = strings.Repeat("b", 64)
	plan.Target.NodePublicKeyFingerprint = fingerprint
	plan.ExpiresAt = now.Add(10 * time.Minute).Format(time.RFC3339)
	state := &memoryState{runtime: RuntimeState{Exchange: client.ExchangeNodeEnrollmentResponse{Plan: plan, Identity: client.NodeEnrollmentIdentity{Generation: 1, SigningKeyID: "node-identity/v1", PublicKeyFingerprint: fingerprint, IssuedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(10 * time.Minute).Format(time.RFC3339)}}}}
	api := &mockJoinAPI{
		credential: client.JoinCredential{IssuanceID: "55555555-5555-4555-8555-555555555555", Credential: strings.Repeat("x", 43), ExpiresAt: now.Add(5 * time.Minute).Format(time.RFC3339), ClusterID: plan.Cluster.ID, WorkerOnly: true},
		node:       client.Node{ID: plan.NodeID, WorkspaceID: plan.WorkspaceID, Name: plan.Hostname, Kind: "shared", Platform: client.NodePlatformLinux, Architecture: client.NodeArchAMD64, LifecycleState: "active", TrustState: "verified", Version: 1, KubernetesBinding: &client.KubernetesBinding{ClusterID: plan.Cluster.ID, NodeName: plan.Hostname, NodeUID: "uid-1", ResourceVersion: "7"}, CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339)},
	}
	coordinator, err := NewBrokerJoinCoordinator(api, state, fixedIdentity{Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.Now = func() time.Time { return now }
	binding, err := coordinator.WorkerCredential(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Credential != api.credential.Credential || binding.ExpectedNodeName != plan.Hostname || api.issueRequest.NodePublicKeyFingerprint != fingerprint || len(api.issueProof) != 86 {
		t.Fatalf("issue binding is incomplete: binding=%+v request=%+v", binding, api.issueRequest)
	}
	joined := JoinedNode{Name: plan.Hostname, UID: "uid-1", ResourceVersion: "7"}
	if err := coordinator.ConfirmJoined(context.Background(), plan, joined); err != nil {
		t.Fatal(err)
	}
	if api.consumeRequest.JoinedNodeUID != joined.UID || api.consumeRequest.ResourceVersion != joined.ResourceVersion || len(api.consumeProof) != 86 {
		t.Fatalf("consume binding is incomplete: %+v", api.consumeRequest)
	}
	if err := coordinator.ConfirmJoined(context.Background(), plan, joined); err == nil {
		t.Fatal("consumed join issuance was reusable")
	}
}

func TestBrokerJoinCoordinatorRejectsCredentialBeyondIdentityExpiry(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, ed25519.SeedSize))
	identity := Identity{PrivateKey: privateKey, PublicKey: privateKey.Public().(ed25519.PublicKey)}
	fingerprint, _ := identity.Fingerprint()
	plan := testJoinPlan("linux")
	plan.PlanID, plan.NodeID = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	plan.EnrollmentID, plan.WorkspaceID = "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"
	plan.Digest, plan.Target.MachineFingerprint, plan.Target.NodePublicKeyFingerprint = "sha256:"+strings.Repeat("a", 64), strings.Repeat("b", 64), fingerprint
	plan.ExpiresAt = now.Add(10 * time.Minute).Format(time.RFC3339)
	state := &memoryState{runtime: RuntimeState{Exchange: client.ExchangeNodeEnrollmentResponse{Plan: plan, Identity: client.NodeEnrollmentIdentity{Generation: 1, PublicKeyFingerprint: fingerprint, IssuedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339)}}}}
	api := &mockJoinAPI{credential: client.JoinCredential{IssuanceID: "55555555-5555-4555-8555-555555555555", Credential: strings.Repeat("x", 43), ExpiresAt: now.Add(2 * time.Minute).Format(time.RFC3339), ClusterID: plan.Cluster.ID, WorkerOnly: true}}
	coordinator, _ := NewBrokerJoinCoordinator(api, state, fixedIdentity{Identity: identity})
	coordinator.Now = func() time.Time { return now }
	if _, err := coordinator.WorkerCredential(context.Background(), plan); err == nil {
		t.Fatal("credential beyond identity expiry was accepted")
	}
}

func TestVerifyMutationDetectsFileDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "material")
	content := []byte("signed material")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	uid, _, ok := fileOwner(info)
	gid, groupOK := fileGroup(info)
	if !ok || !groupOK {
		t.Skip("file ownership is unavailable")
	}
	sum := sha256.Sum256(content)
	mutation := client.NodeInstallMutation{Kind: "file", Target: path, Mode: 0600, UID: uid, GID: gid, Desired: map[string]any{"contentSha256": hex.EncodeToString(sum[:])}}
	engine := NativeRootEngine{Platform: runtime.GOOS}
	if err := engine.verifyMutation(context.Background(), client.NodeInstallPlan{}, mutation, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("drift"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := engine.verifyMutation(context.Background(), client.NodeInstallPlan{}, mutation, nil, nil); err == nil {
		t.Fatal("drifted installed file passed live verification")
	}
}

func TestServiceStateRequiresExactSystemdOutputs(t *testing.T) {
	commands := scriptedExecutor{run: func(_ string, args []string, _ []byte) ([]byte, error) {
		if args[0] == "is-enabled" {
			return []byte("static\n"), nil
		}
		return []byte("activating\n"), nil
	}}
	engine := NativeRootEngine{Platform: "linux", Commands: commands}
	response, err := engine.serviceState(context.Background(), client.NodeInstallService{Manager: "systemd", UnitName: "blazn-node.service"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Service == nil || response.Service.Enabled || response.Service.Active {
		t.Fatalf("non-exact service states were accepted: %+v", response.Service)
	}
}

func TestInstalledSnapVersionUsesExactRevisionColumn(t *testing.T) {
	commands := scriptedExecutor{run: func(_ string, _ []string, _ []byte) ([]byte, error) {
		return []byte("Name Version Rev Tracking Publisher Notes\nmicrok8s v1.35.6 9072 1.35/stable canonical** classic\n"), nil
	}}
	engine := NativeRootEngine{Commands: commands}
	version, err := engine.installedPackageVersion(context.Background(), "microk8s", "snap")
	if err != nil || version != "revision:9072" {
		t.Fatalf("version=%q err=%v", version, err)
	}
}

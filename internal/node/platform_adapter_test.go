package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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

type countingJoinCoordinator struct {
	issues, confirms int
	binding          RootJoinBinding
	confirmed        JoinedNode
}

func (c *countingJoinCoordinator) WorkerCredential(context.Context, client.NodeInstallPlan) (RootJoinBinding, error) {
	c.issues++
	return c.binding, nil
}
func (c *countingJoinCoordinator) ConfirmJoined(_ context.Context, _ client.NodeInstallPlan, joined JoinedNode) error {
	c.confirms++
	c.confirmed = joined
	return nil
}

type functionPrivilegedClient func(context.Context, RootRequest) (RootResponse, error)

func (f functionPrivilegedClient) Call(ctx context.Context, request RootRequest) (RootResponse, error) {
	return f(ctx, request)
}

type functionMaterialResolver func(context.Context, client.NodeInstallComponent) ([]byte, error)

func (f functionMaterialResolver) Resolve(ctx context.Context, component client.NodeInstallComponent) ([]byte, error) {
	return f(ctx, component)
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

func TestRootBootstrapReplaysExchangeAndPersistsTokenFreeAuthority(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux signed profile paths are qualified on Linux")
	}
	authorization, _, signer := validBootstrapAuthorizationWithSigner(t)
	binaryValue := []byte("root-authorized-binary")
	binarySum := sha256.Sum256(binaryValue)
	binarySHA := hex.EncodeToString(binarySum[:])
	plan := authorization.Expected.Plan
	for index := range plan.Components {
		if plan.Components[index].SourceClass == "current_binary" {
			plan.Components[index].SHA256 = binarySHA
		}
	}
	for index := range plan.Mutations {
		if plan.Mutations[index].Kind == "file" {
			plan.Mutations[index].Desired["contentSha256"] = binarySHA
			plan.Mutations[index].DesiredDigest = "sha256:" + binarySHA
		}
	}
	digest, err := client.NodeInstallPlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	plan.Digest = digest
	plan.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer.PrivateKey, []byte("blazn-node-install-plan-v1\n"+digest)))
	authorization.Expected.Plan = plan

	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		if request.Method != http.MethodPost || request.URL.Path != "/v1/node-enrollments/"+authorization.EnrollmentID+"/exchange" || request.Header.Get("Authorization") != "" {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		var replay client.ExchangeNodeEnrollmentRequest
		decoder := json.NewDecoder(request.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&replay) != nil || replay.Token != authorization.Token || replay.NodePublicKey != authorization.NodePublicKey || !sameJSON(replay.KubernetesBinding, authorization.KubernetesBinding) {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		response.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(response).Encode(authorization.Expected)
	}))
	defer server.Close()

	root := testRoot(t)
	profileRoot := filepath.Join(root, "profiles")
	if err := os.Mkdir(profileRoot, 0700); err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(root, "blazn")
	if err := os.WriteFile(binaryPath, binaryValue, 0700); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(profileRoot, "adopt.json")
	profileFile := TrustedProfileFile{SchemaVersion: 1, ID: plan.InstallProfile, ControlPlaneOrigin: server.URL, AllowedClusterOrigins: []string{"https://cluster.example.test"}, AllowedDownloadOrigins: []string{"https://download.example.test"}, AllowedDownloadHostSuffixes: []string{}, AllowedRegistryOrigins: []string{"https://registry.example.test"}, AllowedMutationRoots: []string{"/usr/local/bin", "/etc/systemd/system", "/var/lib/blazn/install-backups"}, EmbeddedComponentSHA256: map[string]string{"service-definition": testHash}}
	profileBytes, _ := json.Marshal(profileFile)
	if err := os.WriteFile(profilePath, profileBytes, 0600); err != nil {
		t.Fatal(err)
	}
	authorization.ProfilePath = profilePath
	when := time.Date(2026, 8, 21, 0, 1, 0, 0, time.UTC)
	liveResourceVersion := authorization.KubernetesBinding.ResourceVersion
	commands := scriptedExecutor{run: func(_ string, _ []string, _ []byte) ([]byte, error) {
		return []byte(fmt.Sprintf(`{"metadata":{"name":%q,"uid":%q,"resourceVersion":%q},"spec":{"taints":[]}}`, plan.Hostname, authorization.KubernetesBinding.NodeUID, liveResourceVersion)), nil
	}}
	engine := NativeRootEngine{Platform: "linux", Commands: commands, AuthorityPath: filepath.Join(root, "authority", "install-authority.json"), ProfileRoot: profileRoot, CurrentBinaryPath: binaryPath, AuthorityHTTPClient: server.Client(), Now: func() time.Time { return when }}
	bootstrap := RootBootstrapRequest{EnrollmentID: authorization.EnrollmentID, Token: authorization.Token, MachineFingerprint: authorization.MachineFingerprint, NodePublicKey: authorization.NodePublicKey, Platform: authorization.Platform, Architecture: authorization.Architecture, KubernetesBinding: authorization.KubernetesBinding, PlanSigningKey: authorization.PlanSigningKey, Expected: authorization.Expected, ProfileID: authorization.ProfileID, ProfilePath: authorization.ProfilePath}
	request := RootRequest{SchemaVersion: RootHelperSchema, Operation: RootAuthorize, Platform: "linux", Plan: plan, Bootstrap: &bootstrap}
	if err := engine.authorizeBootstrap(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	when = when.Add(time.Second)
	if err := engine.authorizeBootstrap(context.Background(), request); err != nil {
		t.Fatalf("idempotent exchange replay failed: %v", err)
	}
	encoded, err := readPrivateFile(engine.AuthorityPath, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(authorization.Token)) || bytes.Contains(encoded, []byte(`"token"`)) {
		t.Fatal("root authority persisted enrollment token material")
	}
	authority, err := DecodeRootInstallAuthority(encoded)
	if err != nil || authority.PlanSigningKey != authorization.PlanSigningKey || !sameJSON(authority.KubernetesBinding, authorization.KubernetesBinding) {
		t.Fatalf("authority=%#v err=%v", authority, err)
	}
	if err := engine.AuthorizeRootRequest(context.Background(), RootRequest{SchemaVersion: RootHelperSchema, Operation: RootVerify, Platform: "linux", Plan: plan}); err != nil {
		t.Fatal(err)
	}
	liveResourceVersion = "99"
	if err := engine.AuthorizeRootRequest(context.Background(), RootRequest{SchemaVersion: RootHelperSchema, Operation: RootVerify, Platform: "linux", Plan: plan}); err != nil {
		t.Fatalf("long-lived authority incorrectly pinned resourceVersion: %v", err)
	}
	if requests != 2 {
		t.Fatalf("exchange replay requests=%d", requests)
	}
}

func TestDefaultRootAuthorityPathsFollowPlatformContract(t *testing.T) {
	linux := NativeRootEngine{Platform: "linux"}
	profile, binary, authority, err := linux.authorityPaths()
	if err != nil || profile != LinuxNodeProfileRoot || binary != defaultRootBinaryPath || authority != "/var/lib/blazn-node-root/install-authority.json" {
		t.Fatalf("linux paths profile=%q binary=%q authority=%q err=%v", profile, binary, authority, err)
	}
	mac := NativeRootEngine{Platform: "macos"}
	profile, binary, authority, err = mac.authorityPaths()
	if err != nil || profile != MacOSNodeProfileRoot || binary != defaultRootBinaryPath || authority != "/Library/Application Support/BlaznNodeRoot/install-authority.json" {
		t.Fatalf("mac paths profile=%q binary=%q authority=%q err=%v", profile, binary, authority, err)
	}
}

func TestClusterMutationUsesResourceVersionAsOperationPrecondition(t *testing.T) {
	plan := testJoinPlan("linux")
	commands := &recordingExecutor{}
	engine := NativeRootEngine{Platform: "linux", Commands: commands}
	mutation := client.NodeInstallMutation{Kind: "label", Target: "blazn.dev/node", Desired: map[string]any{"value": "true"}}
	binding := &RootJoinBinding{ExpectedNodeName: plan.Hostname, ExpectedNodeUID: "uid-1", ExpectedResourceVersion: "6"}
	if err := engine.applyClusterMutation(context.Background(), plan, mutation, binding, false); err == nil || len(commands.calls) != 1 {
		t.Fatalf("stale resourceVersion err=%v calls=%#v", err, commands.calls)
	}
}

func TestRootAuthorityHTTPClientHasNoProxyAndRejectsRedirects(t *testing.T) {
	httpClient := newRootAuthorityHTTPClient()
	transport, ok := httpClient.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || !transport.DisableCompression || transport.TLSClientConfig == nil || transport.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Fatalf("unsafe root authority transport: %#v", httpClient.Transport)
	}
	request, _ := http.NewRequest(http.MethodPost, "https://control.example.test/exchange", nil)
	if err := httpClient.CheckRedirect(request, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect policy error=%v", err)
	}
	if httpClient.Timeout != 30*time.Second {
		t.Fatalf("root authority timeout=%v", httpClient.Timeout)
	}
}

func TestRootHelperResponseRequiresEOF(t *testing.T) {
	valid := `{"schemaVersion":"blazn.dev/node-root-helper/v1","ok":true}`
	if _, err := decodeRootResponse(strings.NewReader(valid)); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRootResponse(strings.NewReader(valid + `{}`)); err == nil {
		t.Fatal("trailing helper response accepted")
	}
}

func TestAdoptedWorkerUsesPinnedBindingWithoutIssuingJoinCredential(t *testing.T) {
	authorization, _ := validBootstrapAuthorization(t)
	plan := authorization.Expected.Plan
	coordinator := &countingJoinCoordinator{}
	resourceVersion := int64(7)
	privileged := functionPrivilegedClient(func(_ context.Context, request RootRequest) (RootResponse, error) {
		switch request.Operation {
		case RootApply:
			resourceVersion++
			return RootResponse{OK: true, KubernetesBinding: &client.KubernetesBinding{ClusterID: plan.Cluster.ID, NodeName: plan.Hostname, NodeUID: "uid-1", ResourceVersion: strconv.FormatInt(resourceVersion, 10)}}, nil
		case RootVerify:
			return RootResponse{OK: true}, nil
		default:
			return RootResponse{}, errors.New("unexpected privileged operation")
		}
	})
	adapter, err := NewPlatformAdapter("linux", privileged, TrustedMaterialResolver{}, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	adapter.plan = plan
	adapter.bootstrapBinding = authorization.KubernetesBinding
	adapter.deferred = []client.NodeInstallMutation{{Ordinal: 4, Kind: "label", Action: "apply", Target: "blazn.dev/node", Desired: map[string]any{"value": "true"}}}
	if err := adapter.Verify(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if coordinator.issues != 0 || coordinator.confirms != 0 || adapter.joined == nil || adapter.joined.ExpectedResourceVersion != "8" {
		t.Fatalf("issues=%d confirms=%d joined=%#v", coordinator.issues, coordinator.confirms, adapter.joined)
	}
}

func TestFreshJoinConsumptionUsesFinalDeferredMutationBinding(t *testing.T) {
	plan := testJoinPlan("linux")
	plan.Mode = client.NodeModeFresh
	coordinator := &countingJoinCoordinator{binding: RootJoinBinding{Credential: strings.Repeat("x", 43), ClusterID: plan.Cluster.ID, ExpectedNodeName: plan.Hostname, BootstrapTaint: plan.Cluster.BootstrapTaint, WorkerOnly: true}}
	version := int64(7)
	privileged := functionPrivilegedClient(func(_ context.Context, request RootRequest) (RootResponse, error) {
		switch request.Operation {
		case RootJoin:
			return RootResponse{OK: true, NodeName: plan.Hostname, NodeUID: "uid-1", ResourceVersion: "7"}, nil
		case RootApply:
			version++
			return RootResponse{OK: true, KubernetesBinding: &client.KubernetesBinding{ClusterID: plan.Cluster.ID, NodeName: plan.Hostname, NodeUID: "uid-1", ResourceVersion: strconv.FormatInt(version, 10)}}, nil
		case RootVerify:
			return RootResponse{OK: true}, nil
		default:
			return RootResponse{}, errors.New("unexpected operation")
		}
	})
	adapter, _ := NewPlatformAdapter("linux", privileged, TrustedMaterialResolver{}, coordinator)
	adapter.deferred = []client.NodeInstallMutation{{Ordinal: 1, Kind: "label"}, {Ordinal: 2, Kind: "taint"}}
	if err := adapter.Verify(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if coordinator.confirms != 1 || coordinator.confirmed.ResourceVersion != "9" || coordinator.confirmed.UID != "uid-1" {
		t.Fatalf("confirmed=%#v count=%d", coordinator.confirmed, coordinator.confirms)
	}
}

func TestRootJoinIntentIsCreateOnceAndCrashResumable(t *testing.T) {
	plan := testJoinPlan("linux")
	plan.Mode = client.NodeModeFresh
	authority := RootInstallAuthority{Plan: plan}
	binding := &RootJoinBinding{ClusterID: plan.Cluster.ID, ExpectedNodeName: plan.Hostname, BootstrapTaint: plan.Cluster.BootstrapTaint, WorkerOnly: true}
	created, err := bindRootJoinIntent(&authority, binding, time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC))
	if err != nil || !created || authority.JoinIntent == nil {
		t.Fatalf("created=%v intent=%#v err=%v", created, authority.JoinIntent, err)
	}
	created, err = bindRootJoinIntent(&authority, binding, time.Time{})
	if err != nil || created {
		t.Fatalf("replay created=%v err=%v", created, err)
	}
	tampered := *binding
	tampered.ExpectedNodeName = "replacement"
	if _, err := bindRootJoinIntent(&authority, &tampered, time.Time{}); err == nil {
		t.Fatal("mismatched crash resume was accepted")
	}
}

func TestPreflightRestoresRootAuthorizedBindingForRecovery(t *testing.T) {
	authorization, _ := validBootstrapAuthorization(t)
	binding := *authorization.KubernetesBinding
	binding.ResourceVersion = "42"
	privileged := functionPrivilegedClient(func(_ context.Context, request RootRequest) (RootResponse, error) {
		if request.Operation != RootProbe {
			return RootResponse{}, errors.New("unexpected operation")
		}
		return RootResponse{OK: true, KubernetesBinding: &binding}, nil
	})
	adapter, err := NewPlatformAdapter("linux", privileged, TrustedMaterialResolver{}, &countingJoinCoordinator{})
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Preflight(context.Background(), authorization.Expected.Plan); err != nil {
		t.Fatal(err)
	}
	if adapter.joined == nil || adapter.joined.ExpectedNodeUID != binding.NodeUID || adapter.joined.ExpectedResourceVersion != "42" {
		t.Fatalf("joined=%#v", adapter.joined)
	}
}

func TestDirectoryRollbackRestoresMetadataWithoutReplacingDirectory(t *testing.T) {
	root := testRoot(t)
	directory := filepath.Join(root, "managed")
	if err := os.Mkdir(directory, 0750); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Lstat(directory)
	uid, _, ownerOK := fileOwner(info)
	gid, groupOK := fileGroup(info)
	if !ownerOK || !groupOK {
		t.Skip("directory ownership unavailable")
	}
	metadata := map[string]string{"kind": "directory", "mode": "750", "uid": strconv.FormatInt(uid, 10), "gid": strconv.FormatInt(gid, 10)}
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := restoreDirectoryMetadata(directory, metadata); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0750 {
		t.Fatalf("directory=%v mode=%v", info.IsDir(), info.Mode().Perm())
	}
}

func TestRollbackCASPreservesHostileFileReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "managed")
	desired := []byte("desired")
	if err := os.WriteFile(path, desired, 0600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	uid, _, _ := fileOwner(info)
	gid, _ := fileGroup(info)
	sum := sha256.Sum256(desired)
	plan := client.NodeInstallPlan{Components: []client.NodeInstallComponent{{Name: "config", SHA256: hex.EncodeToString(sum[:])}}}
	mutation := client.NodeInstallMutation{Kind: "file", Action: "write", Target: path, Mode: 0600, UID: uid, GID: gid, Desired: map[string]any{"sourceComponent": "config", "contentSha256": hex.EncodeToString(sum[:])}}
	if err := os.WriteFile(path, []byte("hostile replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	engine := NativeRootEngine{Platform: "linux", Commands: &recordingExecutor{}}
	err := engine.rollback(context.Background(), plan, mutation, PriorState{State: "absent", Material: client.NodeRollbackMaterial{Kind: "absent"}}, t.TempDir(), nil)
	value, readErr := os.ReadFile(path)
	if err == nil || readErr != nil || string(value) != "hostile replacement" {
		t.Fatalf("err=%v readErr=%v value=%q", err, readErr, value)
	}
}

func TestSystemdRollbackReloadsAfterCASRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blazn-node.service")
	desired := []byte("unit")
	if err := os.WriteFile(path, desired, 0600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	uid, _, _ := fileOwner(info)
	gid, _ := fileGroup(info)
	sum := sha256.Sum256(desired)
	plan := client.NodeInstallPlan{Components: []client.NodeInstallComponent{{Name: "unit", SHA256: hex.EncodeToString(sum[:])}}}
	mutation := client.NodeInstallMutation{Kind: "systemd_unit", Action: "write", Target: path, Mode: 0600, UID: uid, GID: gid, Desired: map[string]any{"sourceComponent": "unit"}}
	commands := &recordingExecutor{}
	engine := NativeRootEngine{Platform: "linux", Commands: commands}
	if err := engine.rollback(context.Background(), plan, mutation, PriorState{State: "absent", Material: client.NodeRollbackMaterial{Kind: "absent"}}, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) || len(commands.calls) != 1 || commands.calls[0].path != "/usr/bin/systemctl" || !containsArgument(commands.calls[0].args, "daemon-reload") {
		t.Fatalf("stat=%v calls=%#v", err, commands.calls)
	}
}

func TestPackageCaptureDistinguishesAbsentFromProbeFailure(t *testing.T) {
	plan := client.NodeInstallPlan{PlanID: "11111111-1111-4111-8111-111111111111"}
	mutation := client.NodeInstallMutation{Ordinal: 1, Kind: "package", Target: "microk8s", Desired: map[string]any{"manager": "snap"}}
	backupRoot := testRoot(t)
	engine := NativeRootEngine{Commands: scriptedExecutor{run: func(string, []string, []byte) ([]byte, error) { return nil, &FixedCommandError{ExitCode: 1} }}}
	prior, err := engine.capture(context.Background(), plan, mutation, backupRoot)
	if err != nil || prior.State != "absent" {
		t.Fatalf("prior=%#v err=%v", prior, err)
	}
	engine.Commands = scriptedExecutor{run: func(string, []string, []byte) ([]byte, error) { return nil, errors.New("probe transport failure") }}
	if _, err := engine.capture(context.Background(), plan, mutation, backupRoot); err == nil {
		t.Fatal("package probe failure was misclassified as absence")
	}
}

func TestLargeHTTPSAndCurrentBinaryMaterialsStayOutOfRootPipe(t *testing.T) {
	plan := client.NodeInstallPlan{Components: []client.NodeInstallComponent{{Name: "microk8s", ArtifactType: "package", SourceClass: "https", SHA256: strings.Repeat("a", 64)}, {Name: "blazn", ArtifactType: "binary", SourceClass: "current_binary", SHA256: strings.Repeat("b", 64)}, {Name: "lima-worker-binding", ArtifactType: "configuration", SourceClass: "embedded", SHA256: strings.Repeat("c", 64)}}}
	resolver := functionMaterialResolver(func(context.Context, client.NodeInstallComponent) ([]byte, error) {
		return nil, errors.New("large material must not be resolved outside root")
	})
	adapter := &PlatformAdapter{Materials: resolver, plan: plan}
	for _, mutation := range []client.NodeInstallMutation{{Desired: map[string]any{"componentName": "microk8s"}}, {Desired: map[string]any{"sourceComponent": "blazn"}}, {Action: "adopt_exact", Desired: map[string]any{"sourceComponent": "lima-worker-binding"}}} {
		material, err := adapter.material(context.Background(), mutation)
		if err != nil || material == nil || material.ContentBase64 != "" {
			t.Fatalf("material=%#v err=%v", material, err)
		}
	}
}

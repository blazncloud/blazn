package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testUUIDA = "11111111-1111-4111-8111-111111111111"
	testUUIDB = "22222222-2222-4222-8222-222222222222"
	testUUIDC = "33333333-3333-4333-8333-333333333333"
	testUUIDD = "44444444-4444-4444-8444-444444444444"
	testHash  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func validNodeInstallPlan() NodeInstallPlan {
	return NodeInstallPlan{
		SchemaVersion: NodeSchemaVersion,
		PlanID:        testUUIDA, NodeID: testUUIDB, EnrollmentID: testUUIDC, WorkspaceID: testUUIDD,
		IdempotencyKey: "install-key-1", ApprovedBy: testUUIDA, ApprovedAt: "2026-08-21T00:00:00Z", Hostname: "worker-1.example.test", Mode: NodeModeFresh, InstallProfile: "ubuntu-26.04-amd64-worker/v1",
		Cluster:       NodeInstallCluster{ID: "cluster-1", WorkerOnly: true, APIServer: "https://cluster.example.test", KubernetesVersion: "v1.36.1", JoinCredentialEndpoint: "/v1/node-service/join-credentials", BootstrapTaint: "blazn.dev/bootstrap=pending:NoSchedule", ExpectedCAFingerprint: "sha256:" + testHash, RegistryEndpoints: []string{"https://registry.example.test"}},
		Target:        NodeInstallTarget{Platform: NodePlatformLinux, Architecture: NodeArchAMD64, MachineFingerprint: testHash, NodePublicKeyFingerprint: "sha256:" + testHash, MinCPU: 1, MinMemoryBytes: 1073741824, MinDiskBytes: 10737418240},
		RegistryTrust: []NodeRegistryTrust{},
		Components:    []NodeInstallComponent{{Name: "kubernetes", ArtifactType: "binary", Version: "1.0", Publisher: "Blazn", SourceHost: "example.test", Source: "https://example.test/kubernetes", SHA256: testHash, Ownership: "install"}},
		NodeService:   NodeInstallService{Manager: "systemd", UnitName: "blazn-node", BinaryPath: "/usr/local/bin/blazn", RunAsUser: "blazn-node", RunAsGroup: "blazn-node", DefinitionSHA256: testHash},
		Labels:        map[string]string{"blazn.dev/pool": "default"}, Taints: []NodeTaint{}, ResourceBounds: NodeResourceBounds{MaxPods: 64, MaxConcurrentAgents: 4},
		Mutations:       []NodeInstallMutation{{Ordinal: 1, Kind: "file", Action: "write", Target: "/etc/blazn/node", Desired: map[string]any{"sourceComponent": "kubernetes", "contentSha256": testHash}, DesiredDigest: "sha256:" + testHash, Mode: 0600, UID: 0, GID: 0, Rollback: "remove_if_owned"}},
		ValidationTests: []string{"binary_digest", "worker_only"}, Rollback: NodeInstallRollback{PreserveUserData: true, PreserveControlPlane: true, AmbiguousOwnership: "recovery_required", BackupRootClass: "linux_var_lib", BackupRoot: "/var/lib/blazn/install-backups/receipt-1"},
		IssuedAt: "2026-08-21T00:00:00Z", ExpiresAt: "2026-08-21T00:10:00Z", SigningKeyID: "node-plan/v1", Digest: "sha256:" + testHash, Signature: strings.Repeat("A", 86),
	}
}

func validNodeCapability() NodeCapability {
	return NodeCapability{Version: 1,
		Host:            NodeHostCapacity{Platform: NodePlatformMacOS, Architecture: NodeArchARM64, CPUMillis: 8000, MemoryBytes: 1024, DiskBytes: 1024, Accelerators: []NodeAccelerator{}, Health: NodeCapabilityHealth{Status: "healthy", ReasonCodes: []string{}}},
		Worker:          NodeWorkerCapacity{Platform: NodePlatformLinux, Architecture: NodeArchARM64, AllocatableCPUMillis: 6000, AllocatableMemoryBytes: 1024, AllocatableDiskBytes: 1024, Labels: map[string]string{"blazn.dev/pool": "local-ai"}, Limits: NodeCapabilityLimits{MaxConcurrentAgents: 4}, Health: NodeCapabilityHealth{Status: "healthy", ReasonCodes: []string{}}, KubernetesBinding: KubernetesBinding{ClusterID: "cluster-1", NodeName: "worker-1", NodeUID: "uid-1", ResourceVersion: "1"}},
		SandboxBackends: []string{"agent-sandbox"}, RuntimeClasses: []string{"gvisor"}, LocalModels: []LocalModelCapability{{RouteID: testUUIDA, DisplayName: "Qwen 3.8", Model: "qwen3.8", Protocol: "openai-chat", EndpointClass: "authenticated_node_tunnel", Capabilities: []string{"text", "streaming"}, DataBoundary: "local", Healthy: true, MaxConcurrency: 2, MaxContextTokens: 32768, MaxOutputTokens: 4096}}}
}

func validNodeResponse() Node {
	return Node{ID: testUUIDA, WorkspaceID: testUUIDD, Name: "worker-1", Kind: "shared", Platform: NodePlatformLinux, Architecture: NodeArchAMD64, LifecycleState: "pending", TrustState: "unverified", AgentEligible: false, Version: 1, CapabilityVersion: nil, Identity: nil, KubernetesBinding: nil, CreatedAt: "2026-08-21T00:00:00Z", UpdatedAt: "2026-08-21T00:00:00Z"}
}

func TestValidateNodeInstallPlanSafetyAndMutationUniqueness(t *testing.T) {
	plan := validNodeInstallPlan()
	if err := ValidateNodeInstallPlan(plan); err != nil {
		t.Fatal(err)
	}
	plan.Rollback.PreserveUserData = false
	if err := ValidateNodeInstallPlan(plan); err == nil {
		t.Fatal("unsafe rollback plan passed validation")
	}
	plan = validNodeInstallPlan()
	plan.Mutations = append(plan.Mutations, plan.Mutations[0])
	if err := ValidateNodeInstallPlan(plan); err == nil || !strings.Contains(err.Error(), "repeats ordinal") {
		t.Fatalf("duplicate mutation error=%v", err)
	}
}

func TestNodeInstallMutationDiscriminators(t *testing.T) {
	mutations := []NodeInstallMutation{
		{Ordinal: 1, Kind: "package", Action: "install", Target: "containerd", Desired: map[string]any{"manager": "apt", "version": "1.2.3", "componentName": "containerd"}, DesiredDigest: "sha256:" + testHash, Rollback: "remove_if_owned"},
		{Ordinal: 1, Kind: "file", Action: "write", Target: "/etc/blazn/config", Desired: map[string]any{"sourceComponent": "kubernetes", "contentSha256": testHash}, DesiredDigest: "sha256:" + testHash, Rollback: "remove_if_owned"},
		{Ordinal: 1, Kind: "certificate", Action: "write", Target: "/etc/blazn/ca.pem", Desired: map[string]any{"sourceComponent": "kubernetes", "contentSha256": testHash}, DesiredDigest: "sha256:" + testHash, Rollback: "remove_if_owned"},
		{Ordinal: 1, Kind: "directory", Action: "create", Target: "/opt/blazn", Desired: map[string]any{}, DesiredDigest: "sha256:" + testHash, Rollback: "remove_if_owned"},
		{Ordinal: 1, Kind: "systemd_unit", Action: "enable", Target: "/etc/systemd/system/blazn-node.service", Desired: map[string]any{"unitName": "blazn-node.service", "sourceComponent": "kubernetes"}, DesiredDigest: "sha256:" + testHash, Rollback: "restore_prior"},
		{Ordinal: 1, Kind: "launchd_unit", Action: "enable", Target: "/Library/LaunchDaemons/com.blazn.node.plist", Desired: map[string]any{"label": "com.blazn.node", "sourceComponent": "kubernetes"}, DesiredDigest: "sha256:" + testHash, Rollback: "restore_prior"},
		{Ordinal: 1, Kind: "image", Action: "pull", Target: "registry.example.test/blazn/node@sha256:" + testHash, Desired: map[string]any{"platform": "linux/amd64", "componentName": "node-image"}, DesiredDigest: "sha256:" + testHash, Rollback: "remove_if_owned"},
		{Ordinal: 1, Kind: "label", Action: "apply", Target: "blazn.dev/pool", Desired: map[string]any{"value": "default"}, DesiredDigest: "sha256:" + testHash, Rollback: "restore_prior"},
		{Ordinal: 1, Kind: "taint", Action: "apply", Target: "blazn.dev/bootstrap", Desired: map[string]any{"value": "pending", "effect": "NoSchedule"}, DesiredDigest: "sha256:" + testHash, Rollback: "restore_prior"},
		{Ordinal: 1, Kind: "firewall", Action: "apply", Target: "blazn:node_api", Desired: map[string]any{"protocol": "tcp", "port": 443, "direction": "egress"}, DesiredDigest: "sha256:" + testHash, Rollback: "restore_prior"},
	}
	for _, mutation := range mutations {
		plan := validNodeInstallPlan()
		plan.Mutations = []NodeInstallMutation{mutation}
		switch mutation.Kind {
		case "package":
			plan.Components = []NodeInstallComponent{{Name: "containerd", ArtifactType: "package", Version: "1.2.3", Publisher: "Blazn", SourceHost: "example.test", Source: "https://example.test/containerd", RepositoryOrigin: "https://packages.example.test", SHA256: testHash, Ownership: "install"}}
		case "image":
			plan.Components = []NodeInstallComponent{{Name: "node-image", ArtifactType: "image", Version: "1.0", Publisher: "Blazn", SourceHost: "example.test", Source: "https://example.test/node-image", RegistryHost: "registry.example.test", OCIReference: mutation.Target, SHA256: testHash, Ownership: "install"}}
		case "launchd_unit":
			plan.InstallProfile = "macos-lima-worker-adopt/v1"
			plan.Mode = NodeModeAdopt
			plan.Target.Platform = NodePlatformMacOS
			plan.Target.Architecture = NodeArchARM64
			plan.NodeService = NodeInstallService{Manager: "launchd", UnitName: "com.blazn.node", BinaryPath: "/usr/local/bin/blazn", RunAsUser: "root", RunAsGroup: "wheel", DefinitionSHA256: testHash}
			plan.Rollback.BackupRootClass = "macos_library_application_support"
			plan.Rollback.BackupRoot = "/Library/Application Support/Blazn/install-backups/receipt-1"
		}
		if err := ValidateNodeInstallPlan(plan); err != nil {
			t.Fatalf("kind=%s valid mutation error=%v", mutation.Kind, err)
		}
		plan.Mutations[0].Action = "invalid"
		if err := ValidateNodeInstallPlan(plan); err == nil {
			t.Fatalf("kind=%s invalid action passed", mutation.Kind)
		}
	}
}

func TestValidateNodeOperationRejectsBroadOrUnsafeParameters(t *testing.T) {
	valid := CreateNodeOperationRequest{Type: "remove", ExpectedVersion: 3, Parameters: json.RawMessage(`{"clusterId":"cluster-1","expectedNodeUid":"uid-1","expectedResourceVersion":"9","confirm":true,"preserveHostData":true}`)}
	if err := ValidateCreateNodeOperationRequest(valid); err != nil {
		t.Fatal(err)
	}
	for _, parameters := range []string{
		`{"clusterId":"cluster-1","expectedNodeUid":"uid-1","expectedResourceVersion":"9","confirm":true,"preserveHostData":false}`,
		`{"clusterId":"cluster-1","expectedNodeUid":"uid-1","expectedResourceVersion":"9","confirm":true,"preserveHostData":true,"selector":"*"}`,
	} {
		request := valid
		request.Parameters = json.RawMessage(parameters)
		if err := ValidateCreateNodeOperationRequest(request); err == nil {
			t.Fatalf("unsafe remove parameters passed: %s", parameters)
		}
	}
}

func TestExchangeNodeEnrollmentUsesBodyAndValidatesPlan(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.RawQuery != "" || request.Header.Get("Authorization") != "" || !strings.HasSuffix(request.URL.Path, "/exchange") {
			t.Fatalf("unsafe exchange request path=%s query=%s auth=%q", request.URL.Path, request.URL.RawQuery, request.Header.Get("Authorization"))
		}
		var input ExchangeNodeEnrollmentRequest
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.Token != strings.Repeat("t", 43) {
			t.Fatalf("exchange body=%#v err=%v", input, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(validNodeInstallPlan())
	}))
	defer server.Close()
	api, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := api.ExchangeNodeEnrollment(context.Background(), testUUIDC, ExchangeNodeEnrollmentRequest{Token: strings.Repeat("t", 43), MachineFingerprint: testHash, NodePublicKey: strings.Repeat("A", 43), Platform: NodePlatformLinux, Architecture: NodeArchAMD64})
	if err != nil || plan.NodeID != testUUIDB {
		t.Fatalf("plan=%#v err=%v", plan, err)
	}
}

func TestHeartbeatUsesOnlyNodeProof(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Blazn-Node-Proof") != "proof" || request.Header.Get("Authorization") != "" {
			t.Fatalf("proof=%q auth=%q", request.Header.Get("X-Blazn-Node-Proof"), request.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	api, _ := New(server.URL, server.Client())
	capability := validNodeCapability()
	digest, err := NodeCapabilityDigest(capability)
	if err != nil {
		t.Fatal(err)
	}
	heartbeat := NodeHeartbeat{NodeID: testUUIDA, IdentityGeneration: 1, BootID: "boot", Sequence: 0, SentAt: "2026-08-21T00:00:00Z", CapabilityDigest: digest, Capability: capability}
	if err := api.SubmitNodeHeartbeat(context.Background(), "proof", heartbeat); err != nil {
		t.Fatal(err)
	}
}

func TestNodeCapabilityValidatesLocalCompanyModel(t *testing.T) {
	capability := validNodeCapability()
	if err := ValidateNodeCapability(capability); err != nil {
		t.Fatal(err)
	}
	capability.LocalModels[0].DataBoundary = "cloud"
	if err := ValidateNodeCapability(capability); err == nil {
		t.Fatal("non-local node model boundary passed")
	}
}

func TestNodeCapabilityRejectsNullRequiredCollections(t *testing.T) {
	capability := validNodeCapability()
	if err := ValidateNodeCapability(capability); err != nil {
		t.Fatal(err)
	}
	capability.RuntimeClasses = nil
	if err := ValidateNodeCapability(capability); err == nil {
		t.Fatal("nil required runtimeClasses passed")
	}
}

func TestAgentEligibleNodeRequiresActiveIdentityCapabilityAndBinding(t *testing.T) {
	node := validNodeResponse()
	node.LifecycleState = "active"
	node.TrustState = "verified"
	node.AgentEligible = true
	node.KubernetesBinding = &KubernetesBinding{ClusterID: "cluster-1", NodeName: "worker-1", NodeUID: "uid-1", ResourceVersion: "1"}
	if err := ValidateNode(node); err == nil {
		t.Fatal("eligible Node without identity/capability passed")
	}
	version := int64(1)
	node.CapabilityVersion = &version
	node.Identity = &NodeIdentity{Generation: 1, PublicKeyFingerprint: "sha256:" + testHash, Status: "active", IssuedAt: "2026-08-21T00:00:00Z", ExpiresAt: "2026-08-22T00:00:00Z"}
	if err := ValidateNode(node); err != nil {
		t.Fatal(err)
	}
	node.Identity.Status = "revoked"
	if err := ValidateNode(node); err == nil {
		t.Fatal("eligible Node with revoked identity passed")
	}
}

func TestNodeOperationRejectsNullParameters(t *testing.T) {
	request := CreateNodeOperationRequest{Type: "pause", ExpectedVersion: 1, Parameters: json.RawMessage(`null`)}
	if err := ValidateCreateNodeOperationRequest(request); err == nil {
		t.Fatal("null parameters passed")
	}
}

func TestNodeMutationsRequireBearerAndContractIdempotencyKey(t *testing.T) {
	api, _ := New("https://example.test", nil)
	request := CreateNodeOperationRequest{Type: "pause", ExpectedVersion: 1, Parameters: json.RawMessage(`{}`)}
	for _, testCase := range []struct{ token, key string }{{"", "valid-key"}, {"token", ""}, {"token", "invalid key"}} {
		if _, err := api.CreateNodeOperation(context.Background(), testCase.token, testUUIDA, testCase.key, request); err == nil {
			t.Fatalf("token=%q key=%q unexpectedly accepted", testCase.token, testCase.key)
		}
	}
}

func TestExchangeRejectsUnknownOrTrailingPlanFields(t *testing.T) {
	for _, suffix := range []string{`,"unexpected":true}`, `} {}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			encoded, _ := json.Marshal(validNodeInstallPlan())
			encoded = encoded[:len(encoded)-1]
			_, _ = w.Write(append(encoded, []byte(suffix)...))
		}))
		api, _ := New(server.URL, server.Client())
		_, err := api.ExchangeNodeEnrollment(context.Background(), testUUIDC, ExchangeNodeEnrollmentRequest{Token: strings.Repeat("t", 43), MachineFingerprint: testHash, NodePublicKey: strings.Repeat("A", 43), Platform: NodePlatformLinux, Architecture: NodeArchAMD64})
		server.Close()
		if err == nil {
			t.Fatalf("unsafe response suffix %q passed", suffix)
		}
	}
}

func TestDecodeNodeInstallPlanRequiresPresentCollections(t *testing.T) {
	encoded, _ := json.Marshal(validNodeInstallPlan())
	var raw map[string]any
	_ = json.Unmarshal(encoded, &raw)
	delete(raw, "components")
	encoded, _ = json.Marshal(raw)
	if _, err := DecodeNodeInstallPlan(bytes.NewReader(encoded)); err == nil || !strings.Contains(err.Error(), "components") {
		t.Fatalf("missing components error=%v", err)
	}
}

func testSigningKey() (ed25519.PublicKey, ed25519.PrivateKey) {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize)).Public().(ed25519.PublicKey), ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
}

func signedNodeInstallPlan(t *testing.T) (NodeInstallPlan, NodeInstallPlanTrust) {
	t.Helper()
	publicKey, privateKey := testSigningKey()
	plan := validNodeInstallPlan()
	nodePublicKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	plan.Target.NodePublicKeyFingerprint, _ = NodePublicKeyFingerprint(nodePublicKey)
	digest, err := NodeInstallPlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	plan.Digest = digest
	plan.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, []byte("blazn-node-install-plan-v1\n"+digest)))
	profile := NodeTrustedInstallProfile{ID: plan.InstallProfile, AllowedClusterOrigins: []string{"https://cluster.example.test"}, AllowedDownloadOrigins: []string{"https://example.test"}, AllowedRegistryOrigins: []string{"https://registry.example.test"}, AllowedMutationRoots: []string{"/usr/local/bin", "/etc/blazn", "/var/lib/blazn/install-backups"}, VerifyNoSymlinkTraversal: func(string) error { return nil }}
	trust := NodeInstallPlanTrust{Now: time.Date(2026, 8, 21, 0, 5, 0, 0, time.UTC), Keyring: NodeSigningKeyring{plan.SigningKeyID: publicKey}, WorkspaceID: plan.WorkspaceID, EnrollmentID: plan.EnrollmentID, NodeID: plan.NodeID, Hostname: plan.Hostname, MachineFingerprint: plan.Target.MachineFingerprint, NodePublicKey: nodePublicKey, Platform: plan.Target.Platform, Architecture: plan.Target.Architecture, IdempotencyKey: plan.IdempotencyKey, Profile: profile}
	return plan, trust
}

func TestVerifyNodeInstallPlanPinsSignatureExpiryAndLocalBindings(t *testing.T) {
	plan, trust := signedNodeInstallPlan(t)
	if err := VerifyNodeInstallPlan(plan, trust); err != nil {
		t.Fatal(err)
	}
	tampered := plan
	tampered.Hostname = "attacker.example.test"
	if err := VerifyNodeInstallPlan(tampered, trust); err == nil {
		t.Fatal("tampered hostname passed signature verification")
	}
	wrongBinding := trust
	wrongBinding.MachineFingerprint = strings.Repeat("b", 64)
	if err := VerifyNodeInstallPlan(plan, wrongBinding); err == nil {
		t.Fatal("wrong trusted machine binding passed")
	}
	wrongPublicKey := trust
	wrongPublicKey.NodePublicKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	if err := VerifyNodeInstallPlan(plan, wrongPublicKey); err == nil {
		t.Fatal("wrong trusted node public key passed")
	}
	expired := trust
	expired.Now = time.Date(2026, 8, 21, 0, 10, 0, 0, time.UTC)
	if err := VerifyNodeInstallPlan(plan, expired); err == nil {
		t.Fatal("expired plan passed")
	}
	wrongSigner := trust
	otherPublic, _, _ := ed25519.GenerateKey(nil)
	wrongSigner.Keyring = NodeSigningKeyring{plan.SigningKeyID: otherPublic}
	if err := VerifyNodeInstallPlan(plan, wrongSigner); err == nil {
		t.Fatal("wrong pinned signer passed")
	}
}

func TestTrustedInstallProfileRejectsOriginsRootsRedirectsAndSymlinks(t *testing.T) {
	plan, trust := signedNodeInstallPlan(t)
	untrustedOrigin := trust
	untrustedOrigin.Profile.AllowedDownloadOrigins = []string{"https://other.example.test"}
	if err := VerifyNodeInstallPlan(plan, untrustedOrigin); err == nil {
		t.Fatal("untrusted component origin passed")
	}
	if err := ValidateNodeComponentRedirect(trust.Profile, plan.Components[0], "https://redirect.example.test/file"); err == nil {
		t.Fatal("cross-host redirect passed")
	}
	symlink := trust
	symlink.Profile.VerifyNoSymlinkTraversal = func(target string) error {
		if target == "/etc/blazn/node" {
			return errors.New("symlink")
		}
		return nil
	}
	if err := VerifyNodeInstallPlan(plan, symlink); err == nil {
		t.Fatal("symlink traversal passed")
	}
	unsafeRoot := trust
	unsafeRoot.Profile.AllowedMutationRoots = []string{"/"}
	if err := VerifyNodeInstallPlan(plan, unsafeRoot); err == nil {
		t.Fatal("root mutation profile passed")
	}
	badPayload := validNodeInstallPlan()
	badPayload.Mutations[0].Desired = map[string]any{"path": "/tmp/arbitrary"}
	if err := ValidateNodeInstallPlan(badPayload); err == nil {
		t.Fatal("untyped mutation payload passed")
	}
	traversal := validNodeInstallPlan()
	traversal.Mutations[0].Target = "/etc/blazn/../passwd"
	if err := ValidateNodeInstallPlan(traversal); err == nil {
		t.Fatal("parent traversal passed")
	}
}

func TestPackageRepositoryAndImageRegistryBindSignedComponentsToProfile(t *testing.T) {
	plan := validNodeInstallPlan()
	plan.Components = []NodeInstallComponent{{Name: "containerd", ArtifactType: "package", Version: "1.2.3", Publisher: "Blazn", SourceHost: "example.test", Source: "https://example.test/containerd", RepositoryOrigin: "https://packages.example.test", SHA256: testHash, Ownership: "install"}}
	plan.Mutations = []NodeInstallMutation{{Ordinal: 1, Kind: "package", Action: "install", Target: "containerd", Desired: map[string]any{"manager": "apt", "version": "1.2.3", "componentName": "containerd"}, DesiredDigest: "sha256:" + testHash, Rollback: "remove_if_owned"}}
	profile := NodeTrustedInstallProfile{ID: plan.InstallProfile, AllowedClusterOrigins: []string{"https://cluster.example.test"}, AllowedDownloadOrigins: []string{"https://example.test"}, AllowedRegistryOrigins: []string{"https://registry.example.test"}, AllowedMutationRoots: []string{"/usr/local/bin", "/var/lib/blazn/install-backups"}, VerifyNoSymlinkTraversal: func(string) error { return nil }}
	if err := ValidateNodeInstallProfile(plan, profile); err == nil {
		t.Fatal("untrusted package repository passed")
	}
	profile.AllowedDownloadOrigins = append(profile.AllowedDownloadOrigins, "https://packages.example.test")
	if err := ValidateNodeInstallProfile(plan, profile); err != nil {
		t.Fatal(err)
	}
	plan.Mutations[0].Desired["version"] = "9.9.9"
	if err := ValidateNodeInstallPlan(plan); err == nil {
		t.Fatal("package version drift from signed component passed")
	}

	imageRef := "registry.example.test/blazn/node@sha256:" + testHash
	plan = validNodeInstallPlan()
	plan.Components = []NodeInstallComponent{{Name: "node-image", ArtifactType: "image", Version: "1.0", Publisher: "Blazn", SourceHost: "example.test", Source: "https://example.test/node-image", RegistryHost: "registry.example.test", OCIReference: imageRef, SHA256: testHash, Ownership: "install"}}
	plan.Mutations = []NodeInstallMutation{{Ordinal: 1, Kind: "image", Action: "pull", Target: imageRef, Desired: map[string]any{"platform": "linux/amd64", "componentName": "node-image"}, DesiredDigest: "sha256:" + testHash, Rollback: "remove_if_owned"}}
	if err := ValidateNodeInstallPlan(plan); err != nil {
		t.Fatal(err)
	}
	plan.Mutations[0].Target = "other.example.test/blazn/node@sha256:" + testHash
	if err := ValidateNodeInstallPlan(plan); err == nil {
		t.Fatal("image registry drift from signed component passed")
	}
}

func validInstallReceipt() NodeInstallReceipt {
	return NodeInstallReceipt{SchemaVersion: NodeSchemaVersion, ReceiptID: testUUIDA, PlanID: testUUIDB, PlanDigest: "sha256:" + testHash, NodeID: testUUIDC, Generation: 1, NodeIdentityGeneration: 1, SignerKind: "node_identity", SignerFingerprint: "sha256:" + testHash, State: "active", CurrentStage: "complete", Owner: NodeReceiptOwner{UID: 0, PID: 10, ProcessStartIdentity: "start-1", Nonce: strings.Repeat("A", 32)}, Binary: NodeReceiptBinary{Path: "/usr/local/bin/blazn", Digest: "sha256:" + testHash}, Service: NodeReceiptService{Manager: "systemd", Name: "blazn-node", DefinitionDigest: "sha256:" + testHash}, Mutations: []NodeReceiptMutation{{Ordinal: 1, Kind: "file", Target: "/etc/blazn/node", PriorState: "absent", RollbackMaterial: NodeRollbackMaterial{Kind: "absent"}, DesiredDigest: "sha256:" + testHash, Status: "applied"}}, Residues: []NodeReceiptResidue{}, CreatedAt: "2026-08-21T00:00:00Z", UpdatedAt: "2026-08-21T00:05:00Z", SigningKeyID: "node-identity/v1", Digest: "sha256:" + testHash, Signature: strings.Repeat("A", 86)}
}

func validOperationReceipt() NodeOperationReceipt {
	generation := int64(1)
	return NodeOperationReceipt{SchemaVersion: NodeSchemaVersion, ReceiptID: testUUIDA, OperationID: testUUIDB, NodeID: testUUIDC, WorkspaceID: testUUIDD, OperationType: "pause", ExpectedNodeVersion: 2, StartedAt: "2026-08-21T00:00:00Z", CompletedAt: "2026-08-21T00:01:00Z", Outcome: "succeeded", KubernetesBefore: nil, KubernetesAfter: nil, Actions: []NodeReceiptAction{}, Residues: []NodeReceiptResidue{}, SignerKind: "node_identity", IdentityGeneration: &generation, SignerFingerprint: "sha256:" + testHash, SigningKeyID: "node-identity/v1", Digest: "sha256:" + testHash, Signature: strings.Repeat("A", 86)}
}

func TestVerifySignedInstallAndOperationReceipts(t *testing.T) {
	publicKey, privateKey := testSigningKey()
	fingerprint, _ := NodePublicKeyFingerprint(publicKey)
	install := validInstallReceipt()
	install.SignerFingerprint = fingerprint
	digest, err := NodeInstallReceiptDigest(install)
	if err != nil {
		t.Fatal(err)
	}
	install.Digest = digest
	install.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, []byte("blazn-node-install-receipt-v1\n"+digest)))
	if err := VerifyNodeInstallReceipt(install, NodeInstallReceiptTrust{PlanID: install.PlanID, PlanDigest: install.PlanDigest, NodeID: install.NodeID, Signer: NodeTrustedSigner{Kind: "node_identity", Status: "active", KeyID: install.SigningKeyID, Generation: install.NodeIdentityGeneration, Fingerprint: fingerprint, PublicKey: publicKey}, BackupRoot: "/var/lib/blazn/install-backups/receipt-1", VerifyNoSymlinkTraversal: func(string) error { return nil }}); err != nil {
		t.Fatal(err)
	}
	operation := validOperationReceipt()
	operation.SignerFingerprint = fingerprint
	digest, err = NodeOperationReceiptDigest(operation)
	if err != nil {
		t.Fatal(err)
	}
	operation.Digest = digest
	operation.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, []byte("blazn-node-operation-receipt-v1\n"+digest)))
	trust := NodeOperationReceiptTrust{OperationID: operation.OperationID, NodeID: operation.NodeID, WorkspaceID: operation.WorkspaceID, OperationType: operation.OperationType, ExpectedNodeVersion: operation.ExpectedNodeVersion, NodeIdentitySigner: &NodeTrustedSigner{Kind: "node_identity", Status: "active", KeyID: operation.SigningKeyID, Generation: *operation.IdentityGeneration, Fingerprint: fingerprint, PublicKey: publicKey}}
	if err := VerifyNodeOperationReceipt(operation, trust); err != nil {
		t.Fatal(err)
	}
	operation.Actions = append(operation.Actions, NodeReceiptAction{Ordinal: 1, Kind: "filesystem", Target: "/tmp/tampered", Outcome: "applied"})
	if err := VerifyNodeOperationReceipt(operation, trust); err == nil {
		t.Fatal("tampered operation receipt passed")
	}
}

func TestReceiptSignerAndStateCoherence(t *testing.T) {
	publicKey, privateKey := testSigningKey()
	fingerprint, _ := NodePublicKeyFingerprint(publicKey)
	install := validInstallReceipt()
	install.SignerFingerprint = fingerprint
	digest, _ := NodeInstallReceiptDigest(install)
	install.Digest = digest
	install.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, []byte("blazn-node-install-receipt-v1\n"+digest)))
	wrongGeneration := NodeInstallReceiptTrust{PlanID: install.PlanID, PlanDigest: install.PlanDigest, NodeID: install.NodeID, Signer: NodeTrustedSigner{Kind: "node_identity", Status: "active", KeyID: install.SigningKeyID, Generation: install.NodeIdentityGeneration + 1, Fingerprint: fingerprint, PublicKey: publicKey}, BackupRoot: "/var/lib/blazn/install-backups/receipt-1", VerifyNoSymlinkTraversal: func(string) error { return nil }}
	if err := VerifyNodeInstallReceipt(install, wrongGeneration); err == nil {
		t.Fatal("wrong active identity generation passed")
	}
	incoherent := validInstallReceipt()
	incoherent.Residues = []NodeReceiptResidue{{Target: "/tmp/residue", ReasonCode: "leftover", SafeMessage: "leftover"}}
	if err := ValidateNodeInstallReceipt(incoherent); err == nil {
		t.Fatal("active receipt with residue passed")
	}
	recovery := validInstallReceipt()
	recovery.State = "recovery_required"
	if err := ValidateNodeInstallReceipt(recovery); err == nil {
		t.Fatal("recovery-required receipt without residue passed")
	}

	controlPublic := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{6}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	controlPrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{6}, ed25519.SeedSize))
	controlFingerprint, _ := NodePublicKeyFingerprint(controlPublic)
	operation := validOperationReceipt()
	operation.SignerKind = "control_plane"
	operation.IdentityGeneration = nil
	operation.SignerFingerprint = controlFingerprint
	operation.SigningKeyID = "control-plane-receipt/v1"
	operation.Outcome = "failed"
	operation.Actions = []NodeReceiptAction{{Ordinal: 1, Kind: "api", Target: "dispatch", Outcome: "failed"}}
	digest, _ = NodeOperationReceiptDigest(operation)
	operation.Digest = digest
	operation.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(controlPrivate, []byte("blazn-node-operation-receipt-v1\n"+digest)))
	trust := NodeOperationReceiptTrust{OperationID: operation.OperationID, NodeID: operation.NodeID, WorkspaceID: operation.WorkspaceID, OperationType: operation.OperationType, ExpectedNodeVersion: operation.ExpectedNodeVersion, ControlPlaneSigner: &NodeTrustedSigner{Kind: "control_plane", KeyID: operation.SigningKeyID, Fingerprint: controlFingerprint, PublicKey: controlPublic}}
	if err := VerifyNodeOperationReceipt(operation, trust); err != nil {
		t.Fatal(err)
	}
	operation.Outcome = "succeeded"
	if err := ValidateNodeOperationReceipt(operation); err == nil {
		t.Fatal("control-plane signer claimed success")
	}
}

func TestOpaqueRollbackLocatorResolvesOnlyBelowPlatformRoot(t *testing.T) {
	linuxRoot := "/var/lib/blazn/install-backups/receipt-1"
	resolved, err := ResolveNodeRollbackLocator(linuxRoot, "receipt-backup://prior_state_1")
	if err != nil || resolved != linuxRoot+"/prior_state_1" {
		t.Fatalf("resolved=%q err=%v", resolved, err)
	}
	macRoot := "/Library/Application Support/Blazn/install-backups/receipt-1"
	resolved, err = ResolveNodeRollbackLocator(macRoot, "receipt-backup://prior_state_1")
	if err != nil || resolved != macRoot+"/prior_state_1" {
		t.Fatalf("mac resolved=%q err=%v", resolved, err)
	}
	for _, locator := range []string{"receipt-backup://../escape", "receipt-backup://nested/path", "receipt-backup://dot.name"} {
		if _, err := ResolveNodeRollbackLocator(linuxRoot, locator); err == nil {
			t.Fatalf("unsafe locator %q passed", locator)
		}
	}
}

func TestCapabilityDigestUsesDomainAndStableCanonicalJSON(t *testing.T) {
	capability := validNodeCapability()
	first, err := NodeCapabilityDigest(capability)
	if err != nil {
		t.Fatal(err)
	}
	capability.Worker.Labels["blazn.dev/another"] = "value"
	second, err := NodeCapabilityDigest(capability)
	if err != nil || first == second {
		t.Fatalf("first=%s second=%s err=%v", first, second, err)
	}
	canonical, err := nodeCanonicalJSON(map[string]any{"b": 2, "a": 1})
	if err != nil || string(canonical) != `{"a":1,"b":2}` {
		t.Fatalf("canonical=%s err=%v", canonical, err)
	}
}

func TestCapabilityRejectsDuplicateLocalRouteIDs(t *testing.T) {
	capability := validNodeCapability()
	capability.LocalModels = append(capability.LocalModels, capability.LocalModels[0])
	if err := ValidateNodeCapability(capability); err == nil || !strings.Contains(err.Error(), "routeId") {
		t.Fatalf("duplicate route error=%v", err)
	}
}

func TestNodeOperationRequiresSignedReceiptForPartialOutcome(t *testing.T) {
	operation := NodeOperation{ID: testUUIDB, NodeID: testUUIDC, Type: "pause", Status: "partial", ExpectedNodeVersion: 2, CreatedAt: "2026-08-21T00:00:00Z"}
	if err := ValidateNodeOperation(operation); err == nil {
		t.Fatal("partial operation without receipt passed")
	}
}

func TestConsumeJoinCredentialUsesNodeProofIdempotencyAndStrictNodeResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Blazn-Node-Proof") != "proof" || request.Header.Get("Idempotency-Key") != "consume-key-1" || !strings.HasSuffix(request.URL.Path, "/"+testUUIDA+"/consume") {
			t.Fatalf("headers/path proof=%q key=%q path=%q", request.Header.Get("X-Blazn-Node-Proof"), request.Header.Get("Idempotency-Key"), request.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(validNodeResponse())
	}))
	defer server.Close()
	api, _ := New(server.URL, server.Client())
	request := ConsumeJoinCredentialRequest{NodeID: testUUIDA, EnrollmentID: testUUIDB, PlanID: testUUIDC, JoinedNodeUID: "uid-1", JoinedNodeName: "worker-1", ResourceVersion: "1", ClusterID: "cluster-1"}
	node, err := api.ConsumeNodeJoinCredential(context.Background(), "proof", testUUIDA, "consume-key-1", request)
	if err != nil || node.ID != testUUIDA {
		t.Fatalf("node=%#v err=%v", node, err)
	}
}

func TestJoinCredentialRequiresWorkerOnlyConst(t *testing.T) {
	credential := JoinCredential{IssuanceID: testUUIDA, Credential: strings.Repeat("x", 43), ExpiresAt: "2026-08-21T00:05:00Z", ClusterID: "cluster-1", WorkerOnly: false}
	if err := ValidateJoinCredential(credential); err == nil {
		t.Fatal("control-plane-capable join credential passed")
	}
}

func TestIssueJoinCredentialRequiresAndSendsStableIdempotencyKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Blazn-Node-Proof") != "proof" || request.Header.Get("Idempotency-Key") != "join-key-1" {
			t.Fatalf("proof=%q key=%q", request.Header.Get("X-Blazn-Node-Proof"), request.Header.Get("Idempotency-Key"))
		}
		_ = json.NewEncoder(w).Encode(JoinCredential{IssuanceID: testUUIDA, Credential: strings.Repeat("x", 43), ExpiresAt: "2026-08-21T00:05:00Z", ClusterID: "cluster-1", WorkerOnly: true, Replayed: false})
	}))
	defer server.Close()
	api, _ := New(server.URL, server.Client())
	request := JoinCredentialRequest{EnrollmentID: testUUIDA, PlanID: testUUIDB, PlanDigest: "sha256:" + testHash, NodeID: testUUIDC, MachineFingerprint: testHash, NodePublicKeyFingerprint: "sha256:" + testHash}
	credential, err := api.IssueNodeJoinCredential(context.Background(), "proof", "join-key-1", request)
	if err != nil || credential.IssuanceID != testUUIDA {
		t.Fatalf("credential=%#v err=%v", credential, err)
	}
	if _, err := api.IssueNodeJoinCredential(context.Background(), "proof", "", request); err == nil {
		t.Fatal("missing issuance idempotency key passed")
	}
}

func TestEnrollmentHMACAndJoinCredentialAESFormats(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	token, tokenHash, err := DeriveNodeEnrollmentToken(key, testUUIDA, testUUIDB, testUUIDC, "enrollment-key-1")
	if err != nil || len(token) != 43 || len(tokenHash) != 64 {
		t.Fatalf("token=%q hash=%q err=%v", token, tokenHash, err)
	}
	if token != "oyG8FGAHnl21bk-XX5A0edmbBsL84NMs9o5ZPBDVEoE" || tokenHash != "5f5f00b27a562d1b2a2ee55ab1bbd4536117b9f4a0f187968d408edca3840cb7" {
		t.Fatalf("enrollment HMAC vector token=%q hash=%q", token, tokenHash)
	}
	replayedToken, replayedHash, err := DeriveNodeEnrollmentToken(key, testUUIDA, testUUIDB, testUUIDC, "enrollment-key-1")
	if err != nil || token != replayedToken || tokenHash != replayedHash {
		t.Fatal("enrollment HMAC was not deterministic")
	}
	otherToken, _, _ := DeriveNodeEnrollmentToken(key, testUUIDA, testUUIDB, testUUIDC, "enrollment-key-2")
	if token == otherToken {
		t.Fatal("enrollment idempotency binding did not affect token")
	}

	context := NodeJoinCredentialContext{WorkspaceID: testUUIDA, EnrollmentID: testUUIDB, PlanID: testUUIDC, NodeID: testUUIDD, IssuanceID: "55555555-5555-4555-8555-555555555555", IdempotencyKey: "join-key-1", RequestDigest: testHash}
	aad, err := NodeJoinCredentialAAD(context)
	wantAAD := "blazn-node-join-credential-v1\n" + testUUIDA + "\n" + testUUIDB + "\n" + testUUIDC + "\n" + testUUIDD + "\n55555555-5555-4555-8555-555555555555\njoin-key-1\n" + testHash
	if err != nil || string(aad) != wantAAD {
		t.Fatalf("aad=%q err=%v", aad, err)
	}
	credential := strings.Repeat("credential-", 5)
	sealed, err := sealNodeJoinCredential(key, bytes.NewReader(bytes.Repeat([]byte{2}, 12)), credential, context)
	if err != nil || len(sealed) != 12+len(credential)+16 || !bytes.Equal(sealed[:12], bytes.Repeat([]byte{2}, 12)) {
		t.Fatalf("sealed length=%d err=%v", len(sealed), err)
	}
	opened, err := OpenNodeJoinCredential(key, sealed, context)
	if err != nil || opened != credential {
		t.Fatalf("opened=%q err=%v", opened, err)
	}
	randomSealed, err := SealNodeJoinCredential(key, credential, context)
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenNodeJoinCredential(key, randomSealed, context); err != nil || opened != credential {
		t.Fatalf("random sealed open=%q err=%v", opened, err)
	}
	wrongContext := context
	wrongContext.RequestDigest = strings.Repeat("b", 64)
	if _, err := OpenNodeJoinCredential(key, sealed, wrongContext); err == nil {
		t.Fatal("wrong AES-GCM AAD passed")
	}
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 1
	if _, err := OpenNodeJoinCredential(key, tampered, context); err == nil {
		t.Fatal("tampered AES-GCM ciphertext passed")
	}
}

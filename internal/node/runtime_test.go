package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blazncloud/blazn/internal/client"
)

const testHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestProductionNodePathsSeparateServiceAndPrivilegedState(t *testing.T) {
	linux, err := NodeProductionPaths(client.NodePlatformLinux)
	if err != nil || linux.ServiceStateRoot != "/var/lib/blazn/node" || linux.RootStateRoot != "/var/lib/blazn-node-root" || linux.ProfileRoot != "/etc/blazn/node/profiles" {
		t.Fatalf("linux=%#v err=%v", linux, err)
	}
	mac, err := NodeProductionPaths(client.NodePlatformMacOS)
	if err != nil || mac.ServiceStateRoot != "/Library/Application Support/Blazn/Node" || mac.RootStateRoot != "/Library/Application Support/BlaznNodeRoot" || mac.ProfileRoot != "/Library/Application Support/BlaznNodeRoot/profiles" {
		t.Fatalf("mac=%#v err=%v", mac, err)
	}
	if linux.ServiceStateRoot == linux.RootStateRoot || mac.ServiceStateRoot == mac.RootStateRoot {
		t.Fatal("service-owned and privileged Node state roots overlap")
	}
	if linux.InstallAuthorityPath() != "/var/lib/blazn-node-root/install-authority.json" || linux.InstallWALPath() != "/var/lib/blazn-node-root/install-wal.json" || linux.InstallReceiptPath() != "/var/lib/blazn-node-root/install-receipt.json" || linux.InstallBackupRoot() != "/var/lib/blazn-node-root/install-backups" {
		t.Fatalf("linux privileged paths=%#v", linux)
	}
	if LinuxMicroK8sIssuerStateRoot != "/var/lib/blazn-node-root/microk8s-worker-issuer" {
		t.Fatalf("issuer state root=%q", LinuxMicroK8sIssuerStateRoot)
	}
	if mac.InstallAuthorityPath() != "/Library/Application Support/BlaznNodeRoot/install-authority.json" || mac.InstallWALPath() != "/Library/Application Support/BlaznNodeRoot/install-wal.json" || mac.InstallReceiptPath() != "/Library/Application Support/BlaznNodeRoot/install-receipt.json" || mac.InstallBackupRoot() != "/Library/Application Support/BlaznNodeRoot/install-backups" {
		t.Fatalf("mac privileged paths=%#v", mac)
	}
}

func TestProductionRuntimeSeparatesServiceAndInstallerState(t *testing.T) {
	paths := ProductionNodePaths{ServiceStateRoot: "/service-state", RootStateRoot: "/root-state", ProfileRoot: "/root-profiles"}
	runtime, err := newProductionCommandRuntime(&mockAPI{}, "access", "v1", &countingJoinCoordinator{}, fixedCapability{}, nil, paths, defaultRootBinaryPath)
	if err != nil {
		t.Fatal(err)
	}
	serviceState, serviceOK := runtime.State.(FileStateStore)
	installerState, installerOK := runtime.InstallerState.(*PrivilegedInstallState)
	identityStore, identityOK := runtime.Identities.(FileIdentityStore)
	if !serviceOK || !installerOK || !identityOK || serviceState.Root != paths.ServiceStateRoot || installerState.Local.(FileStateStore).Root != paths.ServiceStateRoot || identityStore.Path != filepath.Join(paths.ServiceStateRoot, "identity.json") || runtime.TrustedProfileRoot != paths.ProfileRoot {
		t.Fatalf("runtime paths: service=%#v installer=%#v identity=%#v profile=%q", runtime.State, runtime.InstallerState, runtime.Identities, runtime.TrustedProfileRoot)
	}
	_, err = newProductionCommandRuntime(&mockAPI{}, "access", "v1", &countingJoinCoordinator{}, fixedCapability{}, nil, ProductionNodePaths{ServiceStateRoot: "/same", RootStateRoot: "/same", ProfileRoot: "/profiles"}, defaultRootBinaryPath)
	if err == nil {
		t.Fatal("overlapping service and privileged roots were accepted")
	}
}

func TestDaemonRuntimeStartsAndHeartbeatsFromServiceStateWithoutUserToken(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/v1/node-service/heartbeats" || request.Header.Get("Authorization") != "" || request.Header.Get("X-Blazn-Node-Proof") == "" {
			t.Errorf("unexpected daemon request")
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	root := testRoot(t)
	paths := ProductionNodePaths{ServiceStateRoot: filepath.Join(root, "service"), RootStateRoot: filepath.Join(root, "privileged"), ProfileRoot: filepath.Join(root, "profiles")}
	identities := FileIdentityStore{Path: filepath.Join(paths.ServiceStateRoot, "identity.json")}
	identity, err := identities.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	plan := installPlan()
	plan.ExpiresAt = "2026-08-22T13:00:00Z"
	capability := testCapability()
	state := FileStateStore{Root: paths.ServiceStateRoot}
	if err := state.SaveRuntime(RuntimeState{SchemaVersion: 1, ControlPlaneOrigin: server.URL, Pin: EnrollmentPin{SchemaVersion: 1}, Exchange: client.ExchangeNodeEnrollmentResponse{Plan: plan, Identity: client.NodeEnrollmentIdentity{Generation: 1, SigningKeyID: "node-identity/v1", PublicKeyFingerprint: mustFingerprint(t, identity), IssuedAt: plan.IssuedAt, ExpiresAt: plan.ExpiresAt}}, KubernetesBinding: &capability.Worker.KubernetesBinding, UpdatedAt: plan.IssuedAt}); err != nil {
		t.Fatal(err)
	}
	runtime, err := newProductionDaemonCommandRuntime(paths, "v-test", server.Client(), fixedCapability{capability: capability})
	if err != nil || runtime.AccessToken != "" || runtime.Service != nil || runtime.Daemon == nil {
		t.Fatalf("runtime=%#v err=%v", runtime, err)
	}
	runtime.Daemon.now = func() time.Time { return time.Date(2026, 8, 22, 12, 30, 0, 0, time.UTC) }
	if _, err := runtime.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("requests=%d", requests)
	}
}

func TestFileStateRejectsTrailingJSON(t *testing.T) {
	root := filepath.Join(testRoot(t), "state")
	store := FileStateStore{Root: root}
	if err := store.SaveRuntime(RuntimeState{SchemaVersion: 1, ControlPlaneOrigin: "https://control.example.test", Pin: EnrollmentPin{SchemaVersion: 1}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "runtime.json")
	value, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(value, []byte("{}")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadRuntime(); err == nil {
		t.Fatal("trailing runtime JSON was accepted")
	}
}

func TestAuthorizedOwnershipTransitionPreservesPrivateDaemonState(t *testing.T) {
	root := filepath.Join(testRoot(t), "service")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"identity.json", "runtime.json", "enrollment-pin.json"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	uid, gid := int(currentUID()), os.Getgid()
	if err := transitionPrivateStateOwnership(root, uid, gid, map[int64]bool{int64(uid): true}); err != nil {
		t.Fatal(err)
	}
	if err := transitionPrivateStateOwnership(root, uid, gid, map[int64]bool{int64(uid): true}); err != nil {
		t.Fatalf("idempotent transition: %v", err)
	}
	for _, name := range []string{"identity.json", "runtime.json", "enrollment-pin.json"} {
		info, err := os.Lstat(filepath.Join(root, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("%s info=%v err=%v", name, info, err)
		}
	}
}

func TestSystemBinaryPromotionRequiresMatchingActiveNodeReceipt(t *testing.T) {
	value := []byte("old-root-binary")
	sum := sha256.Sum256(value)
	receipt := client.NodeInstallReceipt{State: "active", Binary: client.NodeReceiptBinary{Path: defaultRootBinaryPath, Digest: "sha256:" + fmt.Sprintf("%x", sum)}}
	if !receiptOwnsSystemBinary(receipt, value) {
		t.Fatal("matching receipt ownership was rejected")
	}
	receipt.Binary.Digest = "sha256:" + testHash
	if receiptOwnsSystemBinary(receipt, value) {
		t.Fatal("mismatched receipt authorized binary replacement")
	}
	receipt.Binary.Digest = "sha256:" + fmt.Sprintf("%x", sum)
	receipt.State = "removed"
	if !receiptOwnsSystemBinary(receipt, value) {
		t.Fatal("archived removed receipt did not retain binary ownership")
	}
}

func TestDaemonSudoPolicyAllowsOnlyFixedObservationSubcommand(t *testing.T) {
	plan := installPlan()
	plan.NodeService.RunAsUser = "blazn-node"
	policy := string(nodeObservationPolicy(plan))
	if !strings.Contains(policy, "NOPASSWD: /usr/local/bin/blazn node-root-observe\n") || strings.Contains(policy, " node-root-helper") || strings.Contains(policy, "*") {
		t.Fatalf("policy=%q", policy)
	}
}

func TestPrivilegedStatePropagatesLifecycleCancellation(t *testing.T) {
	seenCancelled := false
	client := functionPrivilegedClient(func(ctx context.Context, _ RootRequest) (RootResponse, error) {
		seenCancelled = errors.Is(ctx.Err(), context.Canceled)
		return RootResponse{OK: true}, nil
	})
	state := &PrivilegedInstallState{Client: client, Local: &memoryState{}, Platform: "linux"}
	state.BindPlan(installPlan())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	state.BindContext(ctx)
	if err := state.SaveWAL(InstallWAL{SchemaVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if !seenCancelled {
		t.Fatal("privileged state discarded lifecycle cancellation")
	}
}

func TestProductionCapabilityUsesPersistedVerifiedBinding(t *testing.T) {
	authorization, _ := validBootstrapAuthorization(t)
	plan := authorization.Expected.Plan
	state := &memoryState{runtime: RuntimeState{SchemaVersion: 1, Exchange: authorization.Expected, KubernetesBinding: authorization.KubernetesBinding}}
	observation := LiveNodeObservation{CPUMillis: plan.Target.MinCPU*1000 + plan.ResourceBounds.ReservedCPUMillis, MemoryBytes: plan.Target.MinMemoryBytes + plan.ResourceBounds.ReservedMemoryBytes, DiskBytes: plan.Target.MinDiskBytes, AllocatableCPUMillis: plan.Target.MinCPU * 1000, AllocatableMemoryBytes: plan.Target.MinMemoryBytes, AllocatableDiskBytes: plan.Target.MinDiskBytes, ServiceActive: true, NodeReady: true, Binding: *authorization.KubernetesBinding, RuntimeClasses: []string{"gvisor"}, SandboxBackends: []string{"kubernetes-agent-sandbox"}, ReasonCodes: []string{}}
	capability, err := (ProductionCapabilityProvider{State: state, Observer: fixedLiveObserver{observation: observation}}).Capability(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capability.Worker.KubernetesBinding != *authorization.KubernetesBinding || capability.Host.Platform != plan.Target.Platform || capability.Host.Architecture != plan.Target.Architecture || capability.Worker.AllocatableCPUMillis != plan.Target.MinCPU*1000 {
		t.Fatalf("capability=%#v", capability)
	}
	state.runtime.KubernetesBinding = nil
	if _, err := (ProductionCapabilityProvider{State: state, Observer: fixedLiveObserver{observation: observation}}).Capability(context.Background()); err == nil {
		t.Fatal("capability accepted missing verified binding")
	}
}

type fixedLiveObserver struct {
	observation LiveNodeObservation
	err         error
}

func (f fixedLiveObserver) Observe(context.Context, client.NodeInstallPlan, client.KubernetesBinding) (LiveNodeObservation, error) {
	return f.observation, f.err
}

func TestProductionCapabilityDegradesAndRejectsIdentityMismatch(t *testing.T) {
	authorization, _ := validBootstrapAuthorization(t)
	plan := authorization.Expected.Plan
	state := &memoryState{runtime: RuntimeState{SchemaVersion: 1, Exchange: authorization.Expected, KubernetesBinding: authorization.KubernetesBinding}}
	observation := LiveNodeObservation{CPUMillis: plan.Target.MinCPU*1000 + plan.ResourceBounds.ReservedCPUMillis, MemoryBytes: plan.Target.MinMemoryBytes + plan.ResourceBounds.ReservedMemoryBytes, DiskBytes: plan.Target.MinDiskBytes, AllocatableCPUMillis: plan.Target.MinCPU * 1000, AllocatableMemoryBytes: plan.Target.MinMemoryBytes, AllocatableDiskBytes: plan.Target.MinDiskBytes, Binding: *authorization.KubernetesBinding, RuntimeClasses: []string{}, SandboxBackends: []string{}, ReasonCodes: []string{"worker_observation_failed"}}
	capability, err := (ProductionCapabilityProvider{State: state, Observer: fixedLiveObserver{observation: observation}}).Capability(context.Background())
	if err != nil || capability.Host.Health.Status != "degraded" || len(capability.Host.Health.ReasonCodes) < 2 {
		t.Fatalf("capability=%#v err=%v", capability, err)
	}
	observation.Binding.NodeUID = "replacement"
	if _, err := (ProductionCapabilityProvider{State: state, Observer: fixedLiveObserver{observation: observation}}).Capability(context.Background()); err == nil {
		t.Fatal("replacement Kubernetes identity was accepted")
	}
}

func TestAgentSandboxControllerAvailabilityRequiresObservedAvailableGeneration(t *testing.T) {
	for name, fixture := range map[string]struct {
		value string
		want  bool
	}{
		"available": {value: `{"metadata":{"generation":4},"status":{"observedGeneration":4,"availableReplicas":1}}`, want: true},
		"stale":     {value: `{"metadata":{"generation":4},"status":{"observedGeneration":3,"availableReplicas":1}}`},
		"zero":      {value: `{"metadata":{"generation":4},"status":{"observedGeneration":4,"availableReplicas":0}}`},
		"missing":   {value: `{}`},
		"malformed": {value: `{`},
	} {
		t.Run(name, func(t *testing.T) {
			if got := agentSandboxControllerAvailable([]byte(fixture.value)); got != fixture.want {
				t.Fatalf("available=%v want=%v fixture=%s", got, fixture.want, fixture.value)
			}
		})
	}
}

func TestProductionMaterialsAndRootHelperUseShippedBinary(t *testing.T) {
	materials := ProductionEmbeddedMaterials()
	for name, want := range map[string]string{"blazn-node-systemd": "f16a9389831f6f08b613c96da5af01293f87b129979f4b4bca012b5bf010c661", "blazn-node-launchd": "228cf51dd546f74b789f7d5e032428447d1e85febadad4b9fd2bf1402dea58dc"} {
		sum := sha256.Sum256(materials[name])
		if fmt.Sprintf("%x", sum) != want {
			t.Fatalf("material %s digest=%x", name, sum)
		}
	}
	if !strings.Contains(string(materials["blazn-node-systemd"]), "User=blazn-node\nGroup=blazn-node\nExecStart=/usr/local/bin/blazn node serve") || !strings.Contains(string(materials["blazn-node-launchd"]), "<key>UserName</key><string>_blazn-node</string>") || !strings.Contains(string(materials["blazn-node-launchd"]), "<key>GroupName</key><string>_blazn-node</string>") || !strings.Contains(string(materials["blazn-node-launchd"]), "<string>node</string><string>serve</string>") {
		t.Fatal("installed service units do not execute node serve under the dedicated identity")
	}
	if DefaultRootHelperPath != defaultRootBinaryPath || RootHelperSubcommand != "node-root-helper" {
		t.Fatalf("helper path=%q subcommand=%q", DefaultRootHelperPath, RootHelperSubcommand)
	}
}

func TestBootstrapAuthorizationNeverSerializesOrFormatsToken(t *testing.T) {
	token := strings.Repeat("s", 43)
	authorization := BootstrapAuthorization{EnrollmentID: "enrollment", Token: token, MachineFingerprint: testHash, NodePublicKey: strings.Repeat("A", 43)}
	encoded, err := json.Marshal(authorization)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{string(encoded), fmt.Sprint(authorization), fmt.Sprintf("%#v", authorization)} {
		if strings.Contains(output, token) {
			t.Fatalf("bootstrap token leaked: %s", output)
		}
	}
}

func TestControlPlaneOriginIsExactHTTPSOrigin(t *testing.T) {
	if !validControlPlaneOrigin("https://control.example.test:8443") {
		t.Fatal("exact HTTPS control-plane origin was rejected")
	}
	if !validControlPlaneOrigin("https://[2001:db8::1]:443") {
		t.Fatal("canonical bracketed IPv6 control-plane origin was rejected")
	}
	for _, value := range []string{"http://control.example.test", "https://user@control.example.test", "https://control.example.test/", "https://control.example.test/path", "https://control.example.test?query", "https://control.example.test#fragment", "https://control.example.test:", "https://control.example.test:0", "https://control.example.test:65536", "https://control.example.test:invalid", "https://2001:db8::1"} {
		if validControlPlaneOrigin(value) {
			t.Fatalf("unsafe control-plane origin %q was accepted", value)
		}
	}
}

func TestInstallerBootstrapAuthorizationFailsClosedBeforePlatform(t *testing.T) {
	platform := &mockPlatform{}
	installer := NewInstaller(platform, &memoryState{})
	err := installer.AuthorizeBootstrap(context.Background(), BootstrapAuthorization{Token: strings.Repeat("s", 43)})
	if err == nil || platform.authorization != nil {
		t.Fatalf("authorization=%#v err=%v", platform.authorization, err)
	}
}

func TestInstallerPassesValidBootstrapAuthorizationOnlyInMemory(t *testing.T) {
	platform := &mockPlatform{}
	installer := NewInstaller(platform, &memoryState{})
	authorization, _ := validBootstrapAuthorization(t)
	if err := installer.AuthorizeBootstrap(context.Background(), authorization); err != nil {
		t.Fatal(err)
	}
	if platform.authorization == nil || platform.authorization.Token != authorization.Token {
		t.Fatal("bootstrap token was not handed directly to the privileged platform")
	}
}

func TestServiceAuthorizesBootstrapBeforeRuntimePersistenceAndInstall(t *testing.T) {
	authorization, identity := validBootstrapAuthorization(t)
	platform := &mockPlatform{authorizeErr: errors.New("injected root authorization failure")}
	state := &memoryState{}
	installer := NewInstaller(platform, state)
	api := &mockAPI{secret: client.NodeEnrollmentSecret{ID: authorization.EnrollmentID, Token: authorization.Token, TokenKeyID: "node-enrollment/v1", PlanSigningKey: authorization.PlanSigningKey, ExpiresAt: authorization.Expected.Plan.ExpiresAt}, exchange: authorization.Expected}
	service := NewService(api, fixedIdentity{Identity: identity}, state, installer)
	service.now = func() time.Time { return time.Date(2026, 8, 21, 0, 1, 0, 0, time.UTC) }
	plan := authorization.Expected.Plan
	profile := trustedBootstrapProfile(plan)
	_, err := service.Enroll(context.Background(), EnrollOptions{AccessToken: "access", WorkspaceID: plan.WorkspaceID, IdempotencyKey: plan.IdempotencyKey, Name: plan.Hostname, Mode: plan.Mode, Platform: plan.Target.Platform, Architecture: plan.Target.Architecture, MachineFingerprint: plan.Target.MachineFingerprint, KubernetesBinding: authorization.KubernetesBinding, Profile: profile, ProfilePath: authorization.ProfilePath}, true)
	if err == nil || platform.authorization == nil || platform.authorization.Token != authorization.Token || state.runtime.SchemaVersion != 0 || platform.applyCalls != 0 {
		t.Fatalf("authorization=%#v runtime=%#v apply=%d err=%v", platform.authorization, state.runtime, platform.applyCalls, err)
	}
}

func TestRootInstallAuthorityDigestIsDomainBoundAndTokenFree(t *testing.T) {
	authorization, _ := validBootstrapAuthorization(t)
	plan := authorization.Expected.Plan
	authority := RootInstallAuthority{SchemaVersion: RootInstallAuthoritySchema, Plan: plan, Identity: authorization.Expected.Identity, PlanSigningKey: authorization.PlanSigningKey, NodePublicKey: authorization.NodePublicKey, KubernetesBinding: authorization.KubernetesBinding, ProfileID: authorization.ProfileID, ProfileSHA256: "sha256:" + testHash, ControlPlaneOrigin: "https://control.example.test", AuthorizedAt: "2026-08-21T00:00:30Z"}
	authority.Digest, _ = RootInstallAuthorityDigest(authority)
	trust := RootInstallAuthorityTrust{Now: time.Date(2026, 8, 21, 0, 1, 0, 0, time.UTC), Profile: trustedBootstrapProfile(plan), ProfileSHA256: authority.ProfileSHA256}
	if err := VerifyRootInstallAuthority(authority, trust); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(authority)
	if strings.Contains(string(encoded), "token") {
		t.Fatalf("root authority unexpectedly contains token material: %s", encoded)
	}
	if _, err := DecodeRootInstallAuthority(encoded); err != nil {
		t.Fatal(err)
	}
	var unknown map[string]any
	_ = json.Unmarshal(encoded, &unknown)
	unknown["enrollmentToken"] = strings.Repeat("s", 43)
	withToken, _ := json.Marshal(unknown)
	if _, err := DecodeRootInstallAuthority(withToken); err == nil {
		t.Fatal("token-like unknown root-authority field was accepted")
	}
	delete(unknown, "enrollmentToken")
	delete(unknown, "kubernetesBinding")
	missingBinding, _ := json.Marshal(unknown)
	if _, err := DecodeRootInstallAuthority(missingBinding); err == nil {
		t.Fatal("root authority with an omitted nullable binding was accepted")
	}
	tamperedBinding := authority
	bindingCopy := *authority.KubernetesBinding
	bindingCopy.NodeUID = "substituted-uid"
	tamperedBinding.KubernetesBinding = &bindingCopy
	if err := VerifyRootInstallAuthority(tamperedBinding, trust); err == nil {
		t.Fatal("tampered root-authority Kubernetes binding was accepted")
	}
	tamperedKey := authority
	otherSigner := testIdentity(t)
	otherFingerprint := mustFingerprint(t, otherSigner)
	tamperedKey.PlanSigningKey = client.NodePlanSigningKey{KeyID: plan.SigningKeyID, PublicKey: otherSigner.PublicBase64(), Fingerprint: otherFingerprint}
	tamperedKey.Digest, _ = RootInstallAuthorityDigest(tamperedKey)
	if err := VerifyRootInstallAuthority(tamperedKey, trust); err == nil {
		t.Fatal("tampered root-authority plan key was accepted")
	}
	tamperedPlan := authority
	tamperedPlan.Plan.Hostname = "attacker.example.test"
	tamperedPlan.Digest, _ = RootInstallAuthorityDigest(tamperedPlan)
	if err := VerifyRootInstallAuthority(tamperedPlan, trust); err == nil {
		t.Fatal("tampered root-authority plan was accepted")
	}
	tamperedSignature := authority
	tamperedSignature.Plan.Signature = strings.Repeat("B", 86)
	tamperedSignature.Digest, _ = RootInstallAuthorityDigest(tamperedSignature)
	if err := VerifyRootInstallAuthority(tamperedSignature, trust); err == nil {
		t.Fatal("tampered root-authority plan signature was accepted")
	}
	expiredTrust := trust
	expiredTrust.Now = time.Date(2026, 8, 21, 0, 11, 0, 0, time.UTC)
	if err := VerifyRootInstallAuthority(authority, expiredTrust); err == nil {
		t.Fatal("expired root-authority plan was accepted")
	}
}

func TestFileIdentityStoreCreatesPrivateStableKey(t *testing.T) {
	path := filepath.Join(testRoot(t), "private", "identity.json")
	store := FileIdentityStore{Path: path, Now: func() time.Time { return time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC) }}
	first, err := store.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	if !first.PublicKey.Equal(second.PublicKey) {
		t.Fatal("identity changed")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%v err=%v", info.Mode(), err)
	}
}

func TestTrustedProfileMeasuresCurrentBinaryAndRejectsSymlink(t *testing.T) {
	root := testRoot(t)
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "blazn")
	profilePath := filepath.Join(root, "profile.json")
	if err := os.WriteFile(binary, []byte("binary"), 0700); err != nil {
		t.Fatal(err)
	}
	profile := `{"schemaVersion":1,"id":"ubuntu-26.04-amd64-worker/v1","controlPlaneOrigin":"https://control.example.test","allowedClusterOrigins":["https://cluster.example.test"],"allowedDownloadOrigins":[],"allowedDownloadHostSuffixes":[],"allowedRegistryOrigins":[],"allowedMutationRoots":["/var/lib/blazn"],"embeddedComponentSha256":{}}`
	if err := os.WriteFile(profilePath, []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	trusted, err := LoadTrustedProfile(profilePath, binary, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if trusted.CurrentBinarySHA256 == "" || trusted.CurrentBinaryVersion != "v1" {
		t.Fatalf("profile=%#v", trusted)
	}
	withoutOrigin := strings.Replace(profile, `"controlPlaneOrigin":"https://control.example.test",`, "", 1)
	if err := os.WriteFile(profilePath, []byte(withoutOrigin), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTrustedProfile(profilePath, binary, "v1"); err == nil {
		t.Fatal("trusted profile without a control-plane origin was accepted")
	}
	if err := os.WriteFile(profilePath, []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(binary, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTrustedProfile(profilePath, link, "v1"); err == nil {
		t.Fatal("symlink binary accepted")
	}
	if err := os.Chmod(root, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTrustedProfile(profilePath, binary, "v1"); err == nil {
		t.Fatal("writable trusted-profile parent accepted")
	}
}

func TestInstallerPersistsSignedReceiptAndRollsBackOnFailure(t *testing.T) {
	for _, failure := range []int{-1, 1} {
		t.Run(map[bool]string{true: "success", false: "rollback"}[failure < 0], func(t *testing.T) {
			platform := &mockPlatform{failAt: failure}
			state := &memoryState{}
			installer := NewInstaller(platform, state)
			installer.now = func() time.Time { return time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC) }
			installer.uid = func() int64 { return 0 }
			installer.processIdentity = func() string { return "process-start-1" }
			identity := testIdentity(t)
			plan := installPlan()
			meta := client.NodeEnrollmentIdentity{Generation: 1, SigningKeyID: "node-identity/v1", PublicKeyFingerprint: mustFingerprint(t, identity), IssuedAt: plan.IssuedAt, ExpiresAt: plan.ExpiresAt}
			receipt, err := installer.Install(context.Background(), plan, meta, identity)
			if failure < 0 {
				if err != nil {
					t.Fatal(err)
				}
				if receipt.State != "active" || state.receipt.State != "active" || state.hasWAL {
					t.Fatalf("receipt=%#v wal=%v", receipt, state.hasWAL)
				}
				trust := client.NodeInstallReceiptTrust{PlanID: plan.PlanID, PlanDigest: plan.Digest, NodeID: plan.NodeID, Signer: client.NodeTrustedSigner{Kind: "node_identity", Status: "active", KeyID: meta.SigningKeyID, Generation: 1, Fingerprint: mustFingerprint(t, identity), PublicKey: identity.PublicKey}, BackupRoot: plan.Rollback.BackupRoot, VerifyNoSymlinkTraversal: func(string) error { return nil }}
				if err := client.VerifyNodeInstallReceipt(receipt, trust); err != nil {
					t.Fatal(err)
				}
				applied := platform.applyCalls
				replayed, err := installer.Install(context.Background(), plan, meta, identity)
				if err != nil || replayed.ReceiptID != receipt.ReceiptID || platform.applyCalls != applied {
					t.Fatalf("replayed=%#v applyCalls=%d err=%v", replayed, platform.applyCalls, err)
				}
			} else {
				if err == nil {
					t.Fatal("expected failure")
				}
				if len(platform.rolledBack) != 2 || state.receipt.State != "removed" || state.hasWAL {
					t.Fatalf("rollback=%v receipt=%#v wal=%v", platform.rolledBack, state.receipt, state.hasWAL)
				}
			}
		})
	}
}

func TestCompletedWALRecoveryNeverRollsBackActiveInstall(t *testing.T) {
	identity := testIdentity(t)
	plan := installPlan()
	meta := client.NodeEnrollmentIdentity{Generation: 1, SigningKeyID: "node-identity/v1", PublicKeyFingerprint: mustFingerprint(t, identity), IssuedAt: plan.IssuedAt, ExpiresAt: plan.ExpiresAt}
	state := &memoryState{hasWAL: true, wal: InstallWAL{SchemaVersion: 1, PlanID: plan.PlanID, PlanDigest: plan.Digest, NodeID: plan.NodeID, Stage: "complete", Owner: client.NodeReceiptOwner{UID: 0, PID: 1, ProcessStartIdentity: "start", Nonce: strings.Repeat("A", 32)}, Mutations: []client.NodeReceiptMutation{{Ordinal: 1, Kind: "group", Target: "blazn-node", PriorState: "absent", RollbackMaterial: client.NodeRollbackMaterial{Kind: "absent"}, DesiredDigest: "sha256:" + testHash, Status: "applied"}, {Ordinal: 2, Kind: "user", Target: "blazn-node", PriorState: "absent", RollbackMaterial: client.NodeRollbackMaterial{Kind: "absent"}, DesiredDigest: "sha256:" + testHash, Status: "applied"}}, CreatedAt: "2026-08-22T12:00:00Z", UpdatedAt: "2026-08-22T12:01:00Z"}}
	state.wal.ReceiptID = "55555555-5555-4555-8555-555555555555"
	state.wal.Generation = 1
	platform := &mockPlatform{failAt: -1}
	installer := NewInstaller(platform, state)
	installer.uid = func() int64 { return 0 }
	installer.now = func() time.Time { return time.Date(2026, 8, 22, 12, 2, 0, 0, time.UTC) }
	receipt, err := installer.Recover(context.Background(), plan, meta, identity)
	if err != nil || receipt.State != "active" || len(platform.rolledBack) != 0 || state.hasWAL {
		t.Fatalf("receipt=%#v rollback=%v wal=%v err=%v", receipt, platform.rolledBack, state.hasWAL, err)
	}
}

func TestFileWALCreateIsExclusive(t *testing.T) {
	store := FileStateStore{Root: filepath.Join(testRoot(t), "state")}
	wal := InstallWAL{SchemaVersion: 1, PlanID: "plan-a"}
	if err := store.CreateWAL(wal); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateWAL(wal); err == nil {
		t.Fatal("concurrent WAL owner replaced")
	}
}

func TestInstallLockAndPrivateStateRejectConcurrentOrLinkedOwners(t *testing.T) {
	root := filepath.Join(testRoot(t), "state")
	store := FileStateStore{Root: root}
	release, err := store.AcquireInstallLock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireInstallLock(); err == nil {
		t.Fatal("concurrent install lock acquired")
	}
	identityPath := filepath.Join(root, "identity.json")
	identityStore := FileIdentityStore{Path: identityPath}
	if _, err := identityStore.LoadOrCreate(); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(identityPath, filepath.Join(root, "identity-link.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := identityStore.LoadOrCreate(); err == nil {
		t.Fatal("hard-linked identity accepted")
	}
	release()
	unsafeRoot := filepath.Join(testRoot(t), "unsafe-state")
	if err := os.MkdirAll(unsafeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(unsafeRoot, "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(unsafeRoot, ".install.lock")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(target)
	if _, err := (FileStateStore{Root: unsafeRoot}).AcquireInstallLock(); err == nil {
		t.Fatal("symlink install lock accepted")
	}
	after, _ := os.Stat(target)
	value, _ := os.ReadFile(target)
	if before.Mode() != after.Mode() || string(value) != "unchanged" {
		t.Fatal("symlink target was modified before lock validation")
	}
}

func TestActiveReceiptReplayRejectsLiveDriftAndRollbackSymlink(t *testing.T) {
	identity := testIdentity(t)
	plan := installPlan()
	meta := client.NodeEnrollmentIdentity{Generation: 1, SigningKeyID: "node-identity/v1", PublicKeyFingerprint: mustFingerprint(t, identity), IssuedAt: plan.IssuedAt, ExpiresAt: plan.ExpiresAt}
	mode, uid, gid := int64(0600), int64(0), int64(0)
	prior := PriorState{State: "preexisting_exact", Material: client.NodeRollbackMaterial{Kind: "metadata_snapshot", Locator: "receipt-backup://55555555-5555-4555-8555-555555555555", Digest: "sha256:" + testHash, Mode: &mode, UID: &uid, GID: &gid}}
	platform := &mockPlatform{failAt: -1, prior: &prior}
	state := &memoryState{}
	installer := NewInstaller(platform, state)
	installer.uid = func() int64 { return 0 }
	installer.now = func() time.Time { return time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC) }
	if _, err := installer.Install(context.Background(), plan, meta, identity); err != nil {
		t.Fatal(err)
	}
	platform.verifyErr = errors.New("live service drift")
	if _, err := installer.Install(context.Background(), plan, meta, identity); err == nil {
		t.Fatal("live drift accepted as idempotent install")
	}
	platform.verifyErr = nil
	installer.verifyNoSymlink = func(string) error { return errors.New("rollback path traverses symlink") }
	if _, err := installer.Install(context.Background(), plan, meta, identity); err == nil {
		t.Fatal("symlinked rollback material accepted on receipt replay")
	}
}

func TestRollbackStopsImmediatelyWhenWALPersistenceFails(t *testing.T) {
	identity := testIdentity(t)
	plan := installPlan()
	meta := client.NodeEnrollmentIdentity{Generation: 1, SigningKeyID: "node-identity/v1", PublicKeyFingerprint: mustFingerprint(t, identity), IssuedAt: plan.IssuedAt, ExpiresAt: plan.ExpiresAt}
	state := &faultState{memoryState: &memoryState{}, failAt: 4}
	platform := &mockPlatform{failAt: 1}
	installer := NewInstaller(platform, state)
	installer.uid = func() int64 { return 0 }
	installer.now = func() time.Time { return time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC) }
	_, err := installer.Install(context.Background(), plan, meta, identity)
	if err == nil || len(platform.rolledBack) != 0 || !state.hasWAL || state.receipt.ReceiptID != "" {
		t.Fatalf("rollback=%v wal=%v receipt=%#v err=%v", platform.rolledBack, state.hasWAL, state.receipt, err)
	}
	receipt, err := installer.Recover(context.Background(), plan, meta, identity)
	if err != nil || receipt.State != "removed" || len(platform.rolledBack) != 2 || !state.hasWAL || state.wal.Checkpoint != "cleanup_pending" || state.wal.TerminalReceipt == nil || !sameJSON(*state.wal.TerminalReceipt, receipt) || state.receipt.ReceiptID != "" {
		t.Fatalf("repeat receipt=%#v rollback=%v wal=%#v rootReceipt=%#v err=%v", receipt, platform.rolledBack, state.wal, state.receipt, err)
	}
}

func TestNodeRepairUninstallAndReinstallLifecycle(t *testing.T) {
	identity := testIdentity(t)
	plan := installPlan()
	meta := client.NodeEnrollmentIdentity{Generation: 1, SigningKeyID: "node-identity/v1", PublicKeyFingerprint: mustFingerprint(t, identity), IssuedAt: plan.IssuedAt, ExpiresAt: plan.ExpiresAt}
	state := &memoryState{}
	platform := &mockPlatform{failAt: -1}
	installer := NewInstaller(platform, state)
	installer.uid = func() int64 { return 0 }
	installer.now = func() time.Time { return time.Date(2026, 8, 22, 12, 1, 0, 0, time.UTC) }
	installed, err := installer.Install(context.Background(), plan, meta, identity)
	if err != nil || installed.State != "active" {
		t.Fatalf("install=%#v err=%v", installed, err)
	}
	repaired, err := installer.Repair(context.Background(), plan, meta, identity)
	if err != nil || repaired.State != "active" || repaired.Generation != installed.Generation+1 {
		t.Fatalf("repair=%#v err=%v", repaired, err)
	}
	removed, err := installer.Uninstall(context.Background(), plan, meta, identity, false)
	if err != nil || removed.State != "removed" || len(platform.rolledBack) != len(plan.Mutations) {
		t.Fatalf("uninstall=%#v rollback=%#v err=%v", removed, platform.rolledBack, err)
	}
	if !state.hasWAL || state.wal.TerminalReceipt == nil || state.receipt.State != "active" {
		t.Fatal("uninstall published removed before cleanup")
	}
	if err := state.SaveReceipt(removed); err != nil {
		t.Fatal(err)
	}
	if err := state.RemoveWAL(); err != nil {
		t.Fatal(err)
	}
	reinstalled, err := installer.Install(context.Background(), plan, meta, identity)
	if err != nil || reinstalled.State != "active" {
		t.Fatalf("reinstall=%#v err=%v", reinstalled, err)
	}
}

func TestFailedRepairRestoresOriginalReceiptAndUninstallEvidence(t *testing.T) {
	identity := testIdentity(t)
	plan := installPlan()
	meta := client.NodeEnrollmentIdentity{Generation: 1, SigningKeyID: "node-identity/v1", PublicKeyFingerprint: mustFingerprint(t, identity), IssuedAt: plan.IssuedAt, ExpiresAt: plan.ExpiresAt}
	state := &memoryState{}
	platform := &mockPlatform{failAt: -1}
	installer := NewInstaller(platform, state)
	installer.uid = func() int64 { return 0 }
	installer.now = func() time.Time { return time.Date(2026, 8, 22, 12, 1, 0, 0, time.UTC) }
	original, err := installer.Install(context.Background(), plan, meta, identity)
	if err != nil {
		t.Fatal(err)
	}
	platform.failAt = platform.applyCalls + 1
	returned, repairErr := installer.Repair(context.Background(), plan, meta, identity)
	if repairErr == nil {
		t.Fatal("failed repair succeeded")
	}
	if returned.State != "active" || !sameJSON(state.receipt, original) || state.hasWAL {
		t.Fatalf("returned=%#v receipt=%#v wal=%v", returned, state.receipt, state.hasWAL)
	}
	removed, err := installer.Uninstall(context.Background(), plan, meta, identity, false)
	if err != nil || removed.State != "removed" {
		t.Fatalf("uninstall after failed repair=%#v err=%v", removed, err)
	}
}

func TestCrashedRepairRecoveryRestoresOriginalActiveReceipt(t *testing.T) {
	identity := testIdentity(t)
	plan := installPlan()
	meta := client.NodeEnrollmentIdentity{Generation: 1, SigningKeyID: "node-identity/v1", PublicKeyFingerprint: mustFingerprint(t, identity), IssuedAt: plan.IssuedAt, ExpiresAt: plan.ExpiresAt}
	state := &memoryState{}
	platform := &mockPlatform{failAt: -1}
	installer := NewInstaller(platform, state)
	installer.uid = func() int64 { return 0 }
	installer.now = func() time.Time { return time.Date(2026, 8, 22, 12, 1, 0, 0, time.UTC) }
	original, err := installer.Install(context.Background(), plan, meta, identity)
	if err != nil {
		t.Fatal(err)
	}
	wal := InstallWAL{SchemaVersion: 1, Lifecycle: "repair", OriginalReceipt: &original, ReceiptID: original.ReceiptID, Generation: original.Generation + 1, PlanID: plan.PlanID, PlanDigest: plan.Digest, NodeID: plan.NodeID, Stage: "configure", Checkpoint: "repair_mutation_1", Owner: client.NodeReceiptOwner{UID: 0, PID: 1, ProcessStartIdentity: "test", Nonce: strings.Repeat("A", 32)}, ServicePrior: ServicePriorState{Enabled: original.Service.PriorEnabled, Active: original.Service.PriorActive}, Mutations: []client.NodeReceiptMutation{{Ordinal: plan.Mutations[0].Ordinal, Kind: plan.Mutations[0].Kind, Target: plan.Mutations[0].Target, PriorState: "absent", RollbackMaterial: client.NodeRollbackMaterial{Kind: "absent"}, DesiredDigest: plan.Mutations[0].DesiredDigest, Status: "pending"}}, CreatedAt: original.CreatedAt, UpdatedAt: original.UpdatedAt}
	if err := state.CreateWAL(wal); err != nil {
		t.Fatal(err)
	}
	recovered, err := installer.Recover(context.Background(), plan, meta, identity)
	if err != nil || !sameJSON(recovered, original) || !sameJSON(state.receipt, original) || state.hasWAL || platform.finalized == 0 {
		t.Fatalf("recovered=%#v receipt=%#v wal=%v finalized=%d err=%v", recovered, state.receipt, state.hasWAL, platform.finalized, err)
	}
}

func TestRepairRecoveryCarriesResiduesAcrossRetries(t *testing.T) {
	identity := testIdentity(t)
	plan := installPlan()
	meta := client.NodeEnrollmentIdentity{Generation: 1, SigningKeyID: "node-identity/v1", PublicKeyFingerprint: mustFingerprint(t, identity), IssuedAt: plan.IssuedAt, ExpiresAt: plan.ExpiresAt}
	state := &memoryState{}
	platform := &mockPlatform{failAt: -1}
	installer := NewInstaller(platform, state)
	installer.uid = func() int64 { return 0 }
	installer.now = func() time.Time { return time.Date(2026, 8, 22, 12, 1, 0, 0, time.UTC) }
	original, err := installer.Install(context.Background(), plan, meta, identity)
	if err != nil {
		t.Fatal(err)
	}
	platform.rollbackErr = errors.New("native rollback blocked")
	mutation := plan.Mutations[0]
	legacy := client.NodeReceiptResidue{Target: "legacy", ReasonCode: "legacy_residue", SafeMessage: "existing residue"}
	wal := InstallWAL{SchemaVersion: 1, Lifecycle: "repair", OriginalReceipt: &original, ReceiptID: original.ReceiptID, Generation: original.Generation + 1, PlanID: plan.PlanID, PlanDigest: plan.Digest, NodeID: plan.NodeID, Stage: "configure", Checkpoint: "repair_mutation_1", Owner: client.NodeReceiptOwner{UID: 0, PID: 1, ProcessStartIdentity: "test", Nonce: strings.Repeat("A", 32)}, Mutations: []client.NodeReceiptMutation{{Ordinal: mutation.Ordinal, Kind: mutation.Kind, Target: mutation.Target, PriorState: "absent", RollbackMaterial: client.NodeRollbackMaterial{Kind: "absent"}, DesiredDigest: mutation.DesiredDigest, Status: "pending"}}, Residues: []client.NodeReceiptResidue{legacy}, CreatedAt: original.CreatedAt, UpdatedAt: original.UpdatedAt}
	if err := state.CreateWAL(wal); err != nil {
		t.Fatal(err)
	}
	if _, err := installer.Recover(context.Background(), plan, meta, identity); err == nil {
		t.Fatal("repair residue reported success")
	}
	first := state.wal
	if !state.hasWAL || first.Checkpoint != "repair_recovery_required" || len(first.Residues) != 2 || !sameJSON(state.receipt, original) {
		t.Fatalf("first wal=%#v receipt=%#v", first, state.receipt)
	}
	platform.rollbackErr = nil
	if _, err := installer.Recover(context.Background(), plan, meta, identity); err == nil {
		t.Fatal("repeated repair residue reported success")
	}
	if !state.hasWAL || len(state.wal.Residues) != 2 || !sameJSON(state.wal.Residues[0], legacy) || !sameJSON(state.receipt, original) {
		t.Fatalf("retry wal=%#v receipt=%#v", state.wal, state.receipt)
	}
}

func TestRepairResidueStatusAndEvidencePersistAtomicallyAcrossCrash(t *testing.T) {
	identity := testIdentity(t)
	plan := installPlan()
	meta := client.NodeEnrollmentIdentity{Generation: 1, SigningKeyID: "node-identity/v1", PublicKeyFingerprint: mustFingerprint(t, identity), IssuedAt: plan.IssuedAt, ExpiresAt: plan.ExpiresAt}
	state := &persistResidueFaultState{memoryState: &memoryState{}}
	platform := &mockPlatform{failAt: -1}
	installer := NewInstaller(platform, state)
	installer.uid = func() int64 { return 0 }
	installer.now = func() time.Time { return time.Date(2026, 8, 22, 12, 1, 0, 0, time.UTC) }
	original, err := installer.Install(context.Background(), plan, meta, identity)
	if err != nil {
		t.Fatal(err)
	}
	platform.rollbackErr = errors.New("rollback blocked")
	mutation := plan.Mutations[0]
	wal := InstallWAL{SchemaVersion: 1, Lifecycle: "repair", OriginalReceipt: &original, ReceiptID: original.ReceiptID, Generation: original.Generation + 1, PlanID: plan.PlanID, PlanDigest: plan.Digest, NodeID: plan.NodeID, Stage: "configure", Checkpoint: "repair_mutation_1", Owner: client.NodeReceiptOwner{UID: 0, PID: 1, ProcessStartIdentity: "test", Nonce: strings.Repeat("A", 32)}, Mutations: []client.NodeReceiptMutation{{Ordinal: mutation.Ordinal, Kind: mutation.Kind, Target: mutation.Target, PriorState: "absent", RollbackMaterial: client.NodeRollbackMaterial{Kind: "absent"}, DesiredDigest: mutation.DesiredDigest, Status: "pending"}}, CreatedAt: original.CreatedAt, UpdatedAt: original.UpdatedAt}
	if err := state.CreateWAL(wal); err != nil {
		t.Fatal(err)
	}
	if _, err := installer.Recover(context.Background(), plan, meta, identity); err == nil {
		t.Fatal("post-save crash was not surfaced")
	}
	if !state.hasWAL || state.wal.Mutations[0].Status != "residue" || len(state.wal.Residues) != 1 || !sameJSON(state.receipt, original) {
		t.Fatalf("atomic persisted wal=%#v receipt=%#v", state.wal, state.receipt)
	}
	platform.rollbackErr = nil
	if _, err := installer.Recover(context.Background(), plan, meta, identity); err == nil {
		t.Fatal("persisted residue disappeared on retry")
	}
	if !state.hasWAL || state.wal.Checkpoint != "repair_recovery_required" || len(state.wal.Residues) != 1 || !sameJSON(state.receipt, original) {
		t.Fatalf("retry wal=%#v receipt=%#v", state.wal, state.receipt)
	}
}

func TestInstallRecoveryHonorsJoinAndPublicationCheckpoints(t *testing.T) {
	for _, tc := range []struct {
		name, checkpoint, status, wantState string
		abortErr                            error
	}{{"issue", "join_intent", "pending", "removed", nil}, {"join", "join", "pending", "removed", nil}, {"binding_quarantined", "binding", "pending", "recovery_required", errors.New("quarantined")}, {"consume", "broker_consume", "applied", "active", nil}, {"consumed", "broker_consumed", "applied", "active", nil}, {"verify", "verify", "applied", "active", nil}, {"receipt", "receipt", "applied", "active", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			identity := testIdentity(t)
			plan := installPlan()
			meta := client.NodeEnrollmentIdentity{Generation: 1, SigningKeyID: "node-identity/v1", PublicKeyFingerprint: mustFingerprint(t, identity), IssuedAt: plan.IssuedAt, ExpiresAt: plan.ExpiresAt}
			state := &memoryState{}
			platform := &checkpointPlatform{mockPlatform: &mockPlatform{failAt: -1}, abortErr: tc.abortErr}
			installer := NewInstaller(platform, state)
			installer.uid = func() int64 { return 0 }
			installer.now = func() time.Time { return time.Date(2026, 8, 22, 12, 1, 0, 0, time.UTC) }
			mutation := plan.Mutations[0]
			wal := InstallWAL{SchemaVersion: 1, Lifecycle: "install", ReceiptID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Generation: 1, PlanID: plan.PlanID, PlanDigest: plan.Digest, NodeID: plan.NodeID, Stage: "install", Checkpoint: tc.checkpoint, Owner: client.NodeReceiptOwner{UID: 0, PID: 1, ProcessStartIdentity: "test", Nonce: strings.Repeat("A", 32)}, Mutations: []client.NodeReceiptMutation{{Ordinal: mutation.Ordinal, Kind: mutation.Kind, Target: mutation.Target, PriorState: "absent", RollbackMaterial: client.NodeRollbackMaterial{Kind: "absent"}, DesiredDigest: mutation.DesiredDigest, Status: tc.status}}, CreatedAt: plan.IssuedAt, UpdatedAt: plan.IssuedAt}
			if err := state.CreateWAL(wal); err != nil {
				t.Fatal(err)
			}
			receipt, err := installer.Recover(context.Background(), plan, meta, identity)
			if tc.wantState == "recovery_required" {
				if err == nil {
					t.Fatal("quarantined join reported success")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if receipt.State != tc.wantState || platform.reconciled != 1 {
				t.Fatalf("receipt=%#v reconciled=%d err=%v", receipt, platform.reconciled, err)
			}
			if tc.wantState == "active" && len(platform.rolledBack) != 0 {
				t.Fatalf("forward checkpoint rolled back: %#v", platform.rolledBack)
			}
			if tc.wantState == "removed" && (platform.finalized != 0 || !state.hasWAL || state.wal.TerminalReceipt == nil) {
				t.Fatal("removed recovery finalized a deleted service identity or lost cleanup WAL")
			}
		})
	}
}

func TestUninstallCleanupJournalResumesLinuxAndMacCrashPoints(t *testing.T) {
	authorization, identity := validBootstrapAuthorization(t)
	basePlan := authorization.Expected.Plan
	state := &memoryState{}
	platform := &mockPlatform{failAt: -1}
	installer := NewInstaller(platform, state)
	installer.uid = func() int64 { return 0 }
	issued, _ := time.Parse(time.RFC3339, basePlan.IssuedAt)
	installer.now = func() time.Time { return issued.Add(time.Minute) }
	if _, err := installer.Install(context.Background(), basePlan, authorization.Expected.Identity, identity); err != nil {
		t.Fatal(err)
	}
	terminal, err := installer.Uninstall(context.Background(), basePlan, authorization.Expected.Identity, identity, false)
	if err != nil || terminal.State != "removed" || !state.hasWAL || state.receipt.State != "active" {
		t.Fatalf("terminal=%#v wal=%v active=%#v err=%v", terminal, state.hasWAL, state.receipt, err)
	}
	baseWAL := state.wal
	for _, platformName := range []client.NodePlatform{client.NodePlatformLinux, client.NodePlatformMacOS} {
		for _, failure := range []struct {
			operation  RootOperation
			checkpoint string
		}{{RootRemoveSupport, ""}, {RootSaveWAL, "cleanup_support_removed"}, {RootSaveWAL, "cleanup_local_state_removed"}, {RootSaveReceipt, ""}, {RootRemoveWAL, ""}} {
			name := string(platformName) + "/" + string(failure.operation) + failure.checkpoint
			t.Run(name, func(t *testing.T) {
				plan := basePlan
				plan.Target.Platform = platformName
				root := filepath.Join(testRoot(t), "service")
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				for _, file := range []string{"identity.json", "runtime.json", "enrollment-pin.json"} {
					if err := os.WriteFile(filepath.Join(root, file), []byte(file), 0600); err != nil {
						t.Fatal(err)
					}
				}
				store := FileStateStore{Root: root}
				journal := UninstallCleanupJournal{SchemaVersion: 1, Plan: plan, Receipt: terminal, CreatedAt: plan.IssuedAt}
				if err := store.CreateUninstallCleanup(journal); err != nil {
					t.Fatal(err)
				}
				wal := baseWAL
				wal.TerminalReceipt = &terminal
				wal.Checkpoint = "cleanup_pending"
				fake := &cleanupPrivileged{wal: &wal, receipt: &state.receipt, failOperation: failure.operation, failCheckpoint: failure.checkpoint}
				runtime := &CommandRuntime{State: store, CleanupClient: fake}
				_, ok, firstErr := runtime.resumePendingUninstallCleanup(context.Background())
				if !ok || firstErr == nil {
					t.Fatalf("injected crash was not observed: ok=%v err=%v", ok, firstErr)
				}
				_, ok, secondErr := runtime.resumePendingUninstallCleanup(context.Background())
				if !ok || secondErr != nil {
					t.Fatalf("cleanup did not resume: ok=%v err=%v", ok, secondErr)
				}
				if fake.wal != nil || fake.receipt == nil || !sameJSON(*fake.receipt, terminal) {
					t.Fatalf("root terminal state wal=%#v receipt=%#v", fake.wal, fake.receipt)
				}
				if _, err := store.LoadUninstallCleanup(); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("cleanup journal remains: %v", err)
				}
				for _, file := range []string{"identity.json", "runtime.json", "enrollment-pin.json"} {
					if _, err := os.Stat(filepath.Join(root, file)); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("local state remains: %s err=%v", file, err)
					}
				}
			})
		}
	}
}

func TestEnrollmentPinIsCreateOnceAndRejectsTrustReplacement(t *testing.T) {
	store := FileStateStore{Root: filepath.Join(testRoot(t), "state")}
	pin := EnrollmentPin{SchemaVersion: 1, EnrollmentID: "11111111-1111-4111-8111-111111111111", PlanSigningKey: client.NodePlanSigningKey{KeyID: "plan/v1", PublicKey: strings.Repeat("A", 43), Fingerprint: "sha256:" + testHash}}
	if err := store.Pin(pin); err != nil {
		t.Fatal(err)
	}
	tampered := pin
	tampered.PlanSigningKey.KeyID = "plan/v2"
	if err := store.Pin(tampered); err == nil {
		t.Fatal("pinned plan signer was replaced")
	}
}

func TestDaemonSignsCanonicalHeartbeatAndAdvancesSequence(t *testing.T) {
	identity := testIdentity(t)
	plan := installPlan()
	capability := testCapability()
	state := &memoryState{runtime: RuntimeState{SchemaVersion: 1, Pin: EnrollmentPin{SchemaVersion: 1}, Exchange: client.ExchangeNodeEnrollmentResponse{Plan: plan, Identity: client.NodeEnrollmentIdentity{Generation: 2, SigningKeyID: "node-identity/v2", PublicKeyFingerprint: mustFingerprint(t, identity), IssuedAt: plan.IssuedAt, ExpiresAt: plan.ExpiresAt}}, KubernetesBinding: &capability.Worker.KubernetesBinding}}
	api := &mockAPI{}
	daemon := NewDaemon(api, state, fixedIdentity{identity}, fixedCapability{capability: capability})
	daemon.now = func() time.Time { return time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC) }
	first, err := daemon.Heartbeat(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := daemon.Heartbeat(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Sequence != 0 || second.Sequence != 1 || first.BootID != second.BootID {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	canonical, err := canonicalJSON(api.lastHeartbeat)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(api.lastProof)
	if err != nil || !ed25519.Verify(identity.PublicKey, append([]byte("blazn-node-heartbeat-v1\n"), canonical...), signature) {
		t.Fatal("heartbeat proof invalid")
	}
}

func TestEnrollmentPinsSignerBeforeRejectingUntrustedPlan(t *testing.T) {
	identity := testIdentity(t)
	signer := testIdentity(t)
	signerFingerprint := mustFingerprint(t, signer)
	api := &mockAPI{secret: client.NodeEnrollmentSecret{ID: "11111111-1111-4111-8111-111111111111", Token: strings.Repeat("t", 43), TokenKeyID: "node-enrollment/v1", PlanSigningKey: client.NodePlanSigningKey{KeyID: "plan/v1", PublicKey: base64.RawURLEncoding.EncodeToString(signer.PublicKey), Fingerprint: signerFingerprint}, ExpiresAt: "2026-08-22T12:15:00Z"}, exchange: client.ExchangeNodeEnrollmentResponse{Plan: client.NodeInstallPlan{}, Identity: client.NodeEnrollmentIdentity{}}}
	state := &memoryState{}
	service := NewService(api, fixedIdentity{identity}, state, nil)
	service.now = func() time.Time { return time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC) }
	_, err := service.Enroll(context.Background(), EnrollOptions{AccessToken: "access", WorkspaceID: "workspace-a", IdempotencyKey: "request-1", Name: "node-a", Mode: client.NodeModeFresh, Platform: client.NodePlatformLinux, Architecture: client.NodeArchAMD64, MachineFingerprint: strings.Repeat("a", 64), Profile: client.NodeTrustedInstallProfile{ID: "ubuntu-26.04-amd64-worker/v1"}}, false)
	if err == nil {
		t.Fatal("untrusted plan accepted")
	}
	if state.pin.PlanSigningKey.KeyID != "plan/v1" || state.runtime.SchemaVersion != 0 {
		t.Fatalf("pin=%#v runtime=%#v", state.pin, state.runtime)
	}
}

type fixedIdentity struct{ Identity }

func (f fixedIdentity) LoadOrCreate() (Identity, error) { return f.Identity, nil }

type fixedCapability struct{ capability client.NodeCapability }

func (f fixedCapability) Capability(context.Context) (client.NodeCapability, error) {
	return f.capability, nil
}

type mockAPI struct {
	lastProof           string
	lastHeartbeat       client.NodeHeartbeat
	lastActivationProof string
	lastActivationKey   string
	lastActivation      client.NodeActivationRequest
	secret              client.NodeEnrollmentSecret
	exchange            client.ExchangeNodeEnrollmentResponse
	activation          client.Node
	activationSigner    *Identity
	activationErr       error
}

func (m *mockAPI) CreateNodeEnrollment(context.Context, string, string, string, client.CreateNodeEnrollmentRequest) (client.NodeEnrollmentSecret, error) {
	if m.secret.ID == "" {
		return client.NodeEnrollmentSecret{}, errors.New("unused")
	}
	return m.secret, nil
}
func (m *mockAPI) ExchangeNodeEnrollment(context.Context, string, client.ExchangeNodeEnrollmentRequest) (client.ExchangeNodeEnrollmentResponse, error) {
	return m.exchange, nil
}
func (m *mockAPI) SubmitNodeHeartbeat(_ context.Context, proof string, heartbeat client.NodeHeartbeat) error {
	m.lastProof = proof
	m.lastHeartbeat = heartbeat
	return nil
}
func (m *mockAPI) ActivateNode(_ context.Context, proof, key string, request client.NodeActivationRequest) (client.NodeActivationResponse, error) {
	m.lastActivationProof = proof
	m.lastActivationKey = key
	m.lastActivation = request
	if m.activationErr != nil {
		return client.NodeActivationResponse{}, m.activationErr
	}
	if m.activationSigner == nil {
		return client.NodeActivationResponse{}, errors.New("activation signer unavailable")
	}
	receiptDigest, _ := client.NodeInstallReceiptDigest(request.Receipt)
	grant := client.NodeActivationGrant{SchemaVersion: client.NodeSchemaVersion, Kind: "node_capacity_activation", NodeID: request.Receipt.NodeID, PlanID: request.Receipt.PlanID, ReceiptDigest: receiptDigest, KubernetesBinding: request.KubernetesBinding, SigningKeyID: m.secret.PlanSigningKey.KeyID}
	grant.Digest, _ = client.NodeActivationGrantDigest(grant)
	grant.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(m.activationSigner.PrivateKey, []byte("blazn-node-capacity-activation-grant-v1\n"+grant.Digest)))
	return client.NodeActivationResponse{Node: m.activation, ActivationGrant: grant}, nil
}

type memoryState struct {
	lock    sync.Mutex
	pin     EnrollmentPin
	runtime RuntimeState
	wal     InstallWAL
	hasWAL  bool
	receipt client.NodeInstallReceipt
}

type faultState struct {
	*memoryState
	failAt int
	saves  int
}
type runtimeFaultState struct {
	*memoryState
	failAt int
	saves  int
}

func (f *runtimeFaultState) SaveRuntime(v RuntimeState) error {
	f.saves++
	if f.saves == f.failAt {
		return errors.New("injected runtime persistence crash")
	}
	return f.memoryState.SaveRuntime(v)
}

type persistResidueFaultState struct {
	*memoryState
	failed bool
}

func (s *persistResidueFaultState) SaveWAL(v InstallWAL) error {
	s.memoryState.SaveWAL(v)
	if !s.failed && len(v.Residues) > 0 {
		for _, mutation := range v.Mutations {
			if mutation.Status == "residue" {
				s.failed = true
				return errors.New("injected crash after atomic residue WAL save")
			}
		}
	}
	return nil
}

func (f *faultState) SaveWAL(v InstallWAL) error {
	f.saves++
	if f.saves == f.failAt {
		return errors.New("injected WAL persistence failure")
	}
	return f.memoryState.SaveWAL(v)
}

func (m *memoryState) AcquireInstallLock() (func(), error) {
	if !m.lock.TryLock() {
		return nil, errors.New("locked")
	}
	return m.lock.Unlock, nil
}

func (m *memoryState) Pin(v EnrollmentPin) error {
	if m.pin.EnrollmentID != "" && !samePin(m.pin, v) {
		return errors.New("pin conflict")
	}
	m.pin = v
	return nil
}
func (m *memoryState) LoadPin() (EnrollmentPin, error) {
	if m.pin.EnrollmentID == "" {
		return EnrollmentPin{}, os.ErrNotExist
	}
	return m.pin, nil
}
func (m *memoryState) SaveRuntime(v RuntimeState) error   { m.runtime = v; return nil }
func (m *memoryState) LoadRuntime() (RuntimeState, error) { return m.runtime, nil }
func (m *memoryState) SaveWAL(v InstallWAL) error         { m.wal = v; m.hasWAL = true; return nil }
func (m *memoryState) CreateWAL(v InstallWAL) error {
	if m.hasWAL {
		return os.ErrExist
	}
	return m.SaveWAL(v)
}
func (m *memoryState) LoadWAL() (InstallWAL, error) {
	if !m.hasWAL {
		return InstallWAL{}, os.ErrNotExist
	}
	return m.wal, nil
}
func (m *memoryState) RemoveWAL() error                              { m.hasWAL = false; return nil }
func (m *memoryState) SaveReceipt(v client.NodeInstallReceipt) error { m.receipt = v; return nil }
func (m *memoryState) LoadReceipt() (client.NodeInstallReceipt, error) {
	if m.receipt.ReceiptID == "" {
		return client.NodeInstallReceipt{}, os.ErrNotExist
	}
	return m.receipt, nil
}

type mockPlatform struct {
	failAt        int
	applyCalls    int
	rolledBack    []int64
	prior         *PriorState
	verifyErr     error
	rollbackErr   error
	finalized     int
	authorizeErr  error
	authorization *BootstrapAuthorization
}
type bindingMockPlatform struct {
	*mockPlatform
	binding      *client.KubernetesBinding
	releaseCount int
	releaseErr   error
}
type checkpointPlatform struct {
	*mockPlatform
	reconcileErr, abortErr error
	reconciled             int
}

func (p *checkpointPlatform) ReconcileRecovery(context.Context, client.NodeInstallPlan) error {
	p.reconciled++
	return p.reconcileErr
}
func (p *checkpointPlatform) AbortIncompleteJoin(context.Context, client.NodeInstallPlan) error {
	return p.abortErr
}

type cleanupPrivileged struct {
	wal            *InstallWAL
	receipt        *client.NodeInstallReceipt
	failOperation  RootOperation
	failCheckpoint string
	failed         bool
}

func (c *cleanupPrivileged) Call(_ context.Context, request RootRequest) (RootResponse, error) {
	matchesOperation := c.failCheckpoint == "" && request.Operation == c.failOperation
	matchesCheckpoint := request.Operation == RootSaveWAL && c.failCheckpoint != "" && request.WAL != nil && request.WAL.Checkpoint == c.failCheckpoint
	if !c.failed && (matchesOperation || matchesCheckpoint) {
		c.failed = true
		return RootResponse{}, errors.New("injected cleanup crash")
	}
	switch request.Operation {
	case RootLoadWAL:
		if c.wal == nil {
			return RootResponse{ErrorCode: "not_found"}, nil
		}
		copy := *c.wal
		return RootResponse{WAL: &copy}, nil
	case RootSaveWAL:
		copy := *request.WAL
		c.wal = &copy
	case RootRemoveWAL:
		c.wal = nil
	case RootSaveReceipt:
		copy := *request.Receipt
		c.receipt = &copy
	case RootLoadReceipt:
		if c.receipt == nil {
			return RootResponse{ErrorCode: "not_found"}, nil
		}
		copy := *c.receipt
		return RootResponse{Receipt: &copy}, nil
	case RootRemoveSupport:
	}
	return RootResponse{OK: true}, nil
}

func (p *bindingMockPlatform) KubernetesBinding() *client.KubernetesBinding {
	if p.binding == nil {
		return nil
	}
	value := *p.binding
	return &value
}

func (p *bindingMockPlatform) ReleaseNodeCapacity(context.Context, client.NodeInstallPlan, client.NodeInstallReceipt, client.NodeActivationGrant) (*client.KubernetesBinding, error) {
	p.releaseCount++
	if p.releaseErr != nil {
		return nil, p.releaseErr
	}
	value := *p.binding
	value.ResourceVersion = "8"
	p.binding = &value
	return &value, nil
}

func (p *bindingMockPlatform) RecoverActivatedCapacity(_ context.Context, _ client.NodeInstallPlan, _ client.NodeInstallReceipt, _ client.NodeActivationGrant, binding client.KubernetesBinding) (*client.KubernetesBinding, error) {
	p.binding = &binding
	return p.ReleaseNodeCapacity(context.Background(), client.NodeInstallPlan{}, client.NodeInstallReceipt{}, client.NodeActivationGrant{})
}

func (m *mockPlatform) AuthorizeBootstrap(_ context.Context, authorization BootstrapAuthorization) error {
	m.authorization = &authorization
	return m.authorizeErr
}
func (*mockPlatform) Preflight(context.Context, client.NodeInstallPlan) error { return nil }
func (*mockPlatform) ServiceState(context.Context, client.NodeInstallService) (ServicePriorState, error) {
	return ServicePriorState{}, nil
}
func (m *mockPlatform) Capture(_ context.Context, _ client.NodeInstallMutation, _ string) (PriorState, error) {
	if m.prior != nil {
		return *m.prior, nil
	}
	return PriorState{State: "absent", Material: client.NodeRollbackMaterial{Kind: "absent"}}, nil
}
func (m *mockPlatform) Apply(_ context.Context, mutation client.NodeInstallMutation) error {
	index := m.applyCalls
	m.applyCalls++
	if index == m.failAt {
		return errors.New("injected apply failure")
	}
	return nil
}
func (m *mockPlatform) Rollback(_ context.Context, mutation client.NodeInstallMutation, _ PriorState) error {
	m.rolledBack = append(m.rolledBack, mutation.Ordinal)
	return m.rollbackErr
}
func (m *mockPlatform) Verify(context.Context, client.NodeInstallPlan) error { return m.verifyErr }
func (m *mockPlatform) FinalizeServiceState(context.Context, client.NodeInstallPlan) error {
	m.finalized++
	return nil
}

func TestInstallPersistsFinalBindingForHeartbeat(t *testing.T) {
	authorization, identity, planSigner := validBootstrapAuthorizationWithSigner(t)
	platform := &bindingMockPlatform{mockPlatform: &mockPlatform{failAt: -1}, binding: authorization.KubernetesBinding}
	state := &memoryState{}
	installer := NewInstaller(platform, state)
	api := &mockAPI{secret: client.NodeEnrollmentSecret{ID: authorization.EnrollmentID, Token: authorization.Token, TokenKeyID: "node-enrollment/v1", PlanSigningKey: authorization.PlanSigningKey, ExpiresAt: authorization.Expected.Plan.ExpiresAt}, exchange: authorization.Expected, activationSigner: &planSigner, activation: client.Node{ID: authorization.Expected.Plan.NodeID, WorkspaceID: authorization.Expected.Plan.WorkspaceID, Name: authorization.Expected.Plan.Hostname, Kind: "shared", Platform: authorization.Expected.Plan.Target.Platform, Architecture: authorization.Expected.Plan.Target.Architecture, LifecycleState: "active", TrustState: "verified", Version: 3, KubernetesBinding: authorization.KubernetesBinding, CreatedAt: "2026-08-21T00:00:00Z", UpdatedAt: "2026-08-21T00:01:00Z"}}
	service := NewService(api, fixedIdentity{identity}, state, installer)
	when := time.Date(2026, 8, 21, 0, 1, 0, 0, time.UTC)
	service.now = func() time.Time { return when }
	profile := trustedBootstrapProfile(authorization.Expected.Plan)
	result, err := service.Enroll(context.Background(), EnrollOptions{AccessToken: "access", WorkspaceID: authorization.Expected.Plan.WorkspaceID, IdempotencyKey: authorization.Expected.Plan.IdempotencyKey, Name: authorization.Expected.Plan.Hostname, Mode: authorization.Expected.Plan.Mode, Platform: authorization.Expected.Plan.Target.Platform, Architecture: authorization.Expected.Plan.Target.Architecture, MachineFingerprint: authorization.MachineFingerprint, KubernetesBinding: authorization.KubernetesBinding, Profile: profile, ProfilePath: authorization.ProfilePath}, true)
	if err != nil || !result.Installed || state.runtime.KubernetesBinding == nil {
		t.Fatalf("result=%#v binding=%#v err=%v", result, state.runtime.KubernetesBinding, err)
	}
	if api.lastActivation.Receipt.ReceiptID != result.Receipt.ReceiptID || api.lastActivation.KubernetesBinding != *authorization.KubernetesBinding || api.lastActivation.ExpectedVersion != 1 || api.lastActivationKey != "node-activate-"+result.Receipt.ReceiptID || api.lastActivationProof == "" {
		t.Fatalf("activation=%#v key=%q proof=%q", api.lastActivation, api.lastActivationKey, api.lastActivationProof)
	}
	if platform.releaseCount != 1 || state.runtime.KubernetesBinding.ResourceVersion != "8" || result.State.KubernetesBinding.ResourceVersion != "8" || platform.finalized != 1 {
		t.Fatalf("release count=%d runtime=%#v result=%#v finalized=%d", platform.releaseCount, state.runtime.KubernetesBinding, result.State.KubernetesBinding, platform.finalized)
	}
	plan := authorization.Expected.Plan
	observation := LiveNodeObservation{CPUMillis: plan.Target.MinCPU*1000 + plan.ResourceBounds.ReservedCPUMillis, MemoryBytes: plan.Target.MinMemoryBytes + plan.ResourceBounds.ReservedMemoryBytes, DiskBytes: plan.Target.MinDiskBytes, AllocatableCPUMillis: plan.Target.MinCPU * 1000, AllocatableMemoryBytes: plan.Target.MinMemoryBytes, AllocatableDiskBytes: plan.Target.MinDiskBytes, ServiceActive: true, NodeReady: true, Binding: *state.runtime.KubernetesBinding, RuntimeClasses: []string{}, SandboxBackends: []string{}, ReasonCodes: []string{}}
	daemon := NewDaemon(api, state, fixedIdentity{identity}, ProductionCapabilityProvider{State: state, Observer: fixedLiveObserver{observation: observation}})
	daemon.now = func() time.Time { return when }
	if _, err := daemon.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestInstallDoesNotReleaseCapacityWhenControlPlaneActivationFails(t *testing.T) {
	authorization, identity := validBootstrapAuthorization(t)
	platform := &bindingMockPlatform{mockPlatform: &mockPlatform{failAt: -1}, binding: authorization.KubernetesBinding}
	state := &memoryState{}
	api := &mockAPI{secret: client.NodeEnrollmentSecret{ID: authorization.EnrollmentID, Token: authorization.Token, TokenKeyID: "node-enrollment/v1", PlanSigningKey: authorization.PlanSigningKey, ExpiresAt: authorization.Expected.Plan.ExpiresAt}, exchange: authorization.Expected, activationErr: errors.New("activation unavailable")}
	service := NewService(api, fixedIdentity{identity}, state, NewInstaller(platform, state))
	service.now = func() time.Time { return time.Date(2026, 8, 21, 0, 1, 0, 0, time.UTC) }
	_, err := service.Enroll(context.Background(), EnrollOptions{AccessToken: "access", WorkspaceID: authorization.Expected.Plan.WorkspaceID, IdempotencyKey: authorization.Expected.Plan.IdempotencyKey, Name: authorization.Expected.Plan.Hostname, Mode: authorization.Expected.Plan.Mode, Platform: authorization.Expected.Plan.Target.Platform, Architecture: authorization.Expected.Plan.Target.Architecture, MachineFingerprint: authorization.MachineFingerprint, KubernetesBinding: authorization.KubernetesBinding, Profile: trustedBootstrapProfile(authorization.Expected.Plan), ProfilePath: authorization.ProfilePath}, true)
	if err == nil || platform.releaseCount != 0 || platform.finalized != 0 || state.runtime.KubernetesBinding == nil || state.runtime.KubernetesBinding.ResourceVersion != "7" {
		t.Fatalf("err=%v release=%d finalized=%d binding=%#v", err, platform.releaseCount, platform.finalized, state.runtime.KubernetesBinding)
	}
}

func TestExpiredActivationRecoveryFinishesAfterGrantPersistedBeforeRelease(t *testing.T) {
	authorization, identity, planSigner := validBootstrapAuthorizationWithSigner(t)
	platform := &bindingMockPlatform{mockPlatform: &mockPlatform{failAt: -1}, binding: authorization.KubernetesBinding, releaseErr: errors.New("injected crash before capacity mutation")}
	state := &memoryState{}
	api := activatedMockAPI(authorization, planSigner)
	service := NewService(api, fixedIdentity{identity}, state, NewInstaller(platform, state))
	service.now = func() time.Time { return time.Date(2026, 8, 21, 0, 1, 0, 0, time.UTC) }
	options := recoveryEnrollOptions(authorization)
	if _, err := service.Enroll(context.Background(), options, true); err == nil || state.runtime.ActivationGrant == nil || platform.releaseCount != 1 {
		t.Fatalf("grant=%#v releases=%d err=%v", state.runtime.ActivationGrant, platform.releaseCount, err)
	}
	platform.releaseErr = nil
	service.now = func() time.Time { return time.Date(2026, 8, 21, 1, 0, 0, 0, time.UTC) }
	result, err := service.Enroll(context.Background(), options, true)
	if err != nil || result.State.KubernetesBinding == nil || result.State.KubernetesBinding.ResourceVersion != "8" || platform.releaseCount != 2 || platform.finalized != 1 {
		t.Fatalf("result=%#v releases=%d finalized=%d err=%v", result, platform.releaseCount, platform.finalized, err)
	}
}

func TestExpiredActivationRecoveryRepairsRuntimeAfterReleasedBindingSaveCrash(t *testing.T) {
	authorization, identity, planSigner := validBootstrapAuthorizationWithSigner(t)
	platform := &bindingMockPlatform{mockPlatform: &mockPlatform{failAt: -1}, binding: authorization.KubernetesBinding}
	state := &runtimeFaultState{memoryState: &memoryState{}, failAt: 4}
	api := activatedMockAPI(authorization, planSigner)
	service := NewService(api, fixedIdentity{identity}, state, NewInstaller(platform, state))
	service.now = func() time.Time { return time.Date(2026, 8, 21, 0, 1, 0, 0, time.UTC) }
	options := recoveryEnrollOptions(authorization)
	if _, err := service.Enroll(context.Background(), options, true); err == nil || state.runtime.ActivationGrant == nil || state.runtime.KubernetesBinding.ResourceVersion != "7" || platform.binding.ResourceVersion != "8" {
		t.Fatalf("runtime=%#v platform=%#v err=%v", state.runtime, platform.binding, err)
	}
	state.failAt = 0
	service.now = func() time.Time { return time.Date(2026, 8, 21, 1, 0, 0, 0, time.UTC) }
	result, err := service.Enroll(context.Background(), options, true)
	if err != nil || result.State.KubernetesBinding == nil || result.State.KubernetesBinding.ResourceVersion != "8" || platform.releaseCount != 2 || platform.finalized != 1 {
		t.Fatalf("result=%#v releases=%d finalized=%d err=%v", result, platform.releaseCount, platform.finalized, err)
	}
}

func activatedMockAPI(authorization BootstrapAuthorization, planSigner Identity) *mockAPI {
	return &mockAPI{secret: client.NodeEnrollmentSecret{ID: authorization.EnrollmentID, Token: authorization.Token, TokenKeyID: "node-enrollment/v1", PlanSigningKey: authorization.PlanSigningKey, ExpiresAt: authorization.Expected.Plan.ExpiresAt}, exchange: authorization.Expected, activationSigner: &planSigner, activation: client.Node{ID: authorization.Expected.Plan.NodeID, WorkspaceID: authorization.Expected.Plan.WorkspaceID, Name: authorization.Expected.Plan.Hostname, Kind: "shared", Platform: authorization.Expected.Plan.Target.Platform, Architecture: authorization.Expected.Plan.Target.Architecture, LifecycleState: "active", TrustState: "verified", Version: 3, KubernetesBinding: authorization.KubernetesBinding, CreatedAt: "2026-08-21T00:00:00Z", UpdatedAt: "2026-08-21T00:01:00Z"}}
}

func recoveryEnrollOptions(authorization BootstrapAuthorization) EnrollOptions {
	plan := authorization.Expected.Plan
	return EnrollOptions{AccessToken: "access", WorkspaceID: plan.WorkspaceID, IdempotencyKey: plan.IdempotencyKey, Name: plan.Hostname, Mode: plan.Mode, Platform: plan.Target.Platform, Architecture: plan.Target.Architecture, MachineFingerprint: plan.Target.MachineFingerprint, KubernetesBinding: authorization.KubernetesBinding, Profile: trustedBootstrapProfile(plan), ProfilePath: authorization.ProfilePath}
}

func testRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(resolved, 0700); err != nil {
		t.Fatal(err)
	}
	return resolved
}

func testIdentity(t *testing.T) Identity {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return Identity{PrivateKey: privateKey, PublicKey: publicKey}
}

func validBootstrapAuthorization(t *testing.T) (BootstrapAuthorization, Identity) {
	authorization, identity, _ := validBootstrapAuthorizationWithSigner(t)
	return authorization, identity
}

func validBootstrapAuthorizationWithSigner(t *testing.T) (BootstrapAuthorization, Identity, Identity) {
	t.Helper()
	nodeIdentity := testIdentity(t)
	nodeFingerprint := mustFingerprint(t, nodeIdentity)
	planSigner := testIdentity(t)
	planFingerprint := mustFingerprint(t, planSigner)
	plan := client.NodeInstallPlan{
		SchemaVersion: client.NodeSchemaVersion, PlanID: "11111111-1111-4111-8111-111111111111", NodeID: "22222222-2222-4222-8222-222222222222", EnrollmentID: "33333333-3333-4333-8333-333333333333", WorkspaceID: "44444444-4444-4444-8444-444444444444",
		IdempotencyKey: "install-key-1", ApprovedBy: "11111111-1111-4111-8111-111111111111", ApprovedAt: "2026-08-21T00:00:00Z", Hostname: "worker-1.example.test", Mode: client.NodeModeAdopt, InstallProfile: "existing-linux-worker-adopt/v1",
		Cluster: client.NodeInstallCluster{ID: "cluster-1", WorkerOnly: true, APIServer: "https://cluster.example.test", KubernetesVersion: "v1.36.1", JoinCredentialEndpoint: "/v1/node-service/join-credentials", BootstrapTaint: "blazn.dev/bootstrap=pending:NoSchedule", ExpectedCAFingerprint: "sha256:" + testHash, RegistryEndpoints: []string{"https://registry.example.test"}},
		Target:  client.NodeInstallTarget{Platform: client.NodePlatformLinux, Architecture: client.NodeArchAMD64, MachineFingerprint: testHash, NodePublicKeyFingerprint: nodeFingerprint, MinCPU: 1, MinMemoryBytes: 1073741824, MinDiskBytes: 10737418240}, RegistryTrust: []client.NodeRegistryTrust{},
		Components:  []client.NodeInstallComponent{{Name: "kubernetes", ArtifactType: "binary", SourceClass: "current_binary", Version: "1.0", Publisher: "Blazn", SHA256: testHash, Ownership: "adopt_exact"}, {Name: "service-definition", ArtifactType: "configuration", SourceClass: "embedded", Version: "1.0", Publisher: "Blazn", SHA256: testHash, Ownership: "adopt_exact"}},
		NodeService: client.NodeInstallService{Manager: "systemd", UnitName: "blazn-node.service", BinaryPath: "/usr/local/bin/blazn", RunAsUser: "blazn-node", RunAsGroup: "blazn-node", DefinitionSHA256: testHash}, Labels: map[string]string{"blazn.dev/pool": "default"}, Taints: []client.NodeTaint{}, ResourceBounds: client.NodeResourceBounds{MaxPods: 64, MaxConcurrentAgents: 4},
		Mutations:       []client.NodeInstallMutation{{Ordinal: 1, Kind: "file", Action: "adopt_exact", Target: "/usr/local/bin/blazn", Desired: map[string]any{"sourceComponent": "kubernetes", "contentSha256": testHash}, DesiredDigest: "sha256:" + testHash, Mode: 0755, UID: 0, GID: 0, Rollback: "leave_and_report"}, {Ordinal: 2, Kind: "systemd_unit", Action: "adopt_exact", Target: "/etc/systemd/system/blazn-node.service", Desired: map[string]any{"unitName": "blazn-node.service", "sourceComponent": "service-definition"}, DesiredDigest: "sha256:" + testHash, Mode: 0644, UID: 0, GID: 0, Rollback: "leave_and_report"}, {Ordinal: 3, Kind: "systemd_unit", Action: "enable", Target: "/etc/systemd/system/blazn-node.service", Desired: map[string]any{"unitName": "blazn-node.service", "sourceComponent": "service-definition"}, DesiredDigest: "sha256:" + testHash, UID: 0, GID: 0, Rollback: "restore_prior"}},
		ValidationTests: []string{"binary_digest", "service_active", "worker_only"}, Rollback: client.NodeInstallRollback{PreserveUserData: true, PreserveControlPlane: true, AmbiguousOwnership: "recovery_required", BackupRootClass: "linux_node_root", BackupRoot: "/var/lib/blazn-node-root/install-backups/receipt-1"},
		IssuedAt: "2026-08-21T00:00:00Z", ExpiresAt: "2026-08-21T00:10:00Z", SigningKeyID: "node-plan/v1", Digest: "sha256:" + testHash, Signature: strings.Repeat("A", 86),
	}
	digest, err := client.NodeInstallPlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	plan.Digest = digest
	plan.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(planSigner.PrivateKey, []byte("blazn-node-install-plan-v1\n"+digest)))
	return BootstrapAuthorization{EnrollmentID: plan.EnrollmentID, Token: strings.Repeat("s", 43), MachineFingerprint: plan.Target.MachineFingerprint, NodePublicKey: nodeIdentity.PublicBase64(), Platform: plan.Target.Platform, Architecture: plan.Target.Architecture, KubernetesBinding: &client.KubernetesBinding{ClusterID: plan.Cluster.ID, NodeName: plan.Hostname, NodeUID: "uid-1", ResourceVersion: "7"}, PlanSigningKey: client.NodePlanSigningKey{KeyID: plan.SigningKeyID, PublicKey: planSigner.PublicBase64(), Fingerprint: planFingerprint}, Expected: client.ExchangeNodeEnrollmentResponse{Plan: plan, Identity: client.NodeEnrollmentIdentity{Generation: 1, SigningKeyID: "node-identity/v1", PublicKeyFingerprint: nodeFingerprint, IssuedAt: plan.IssuedAt, ExpiresAt: plan.ExpiresAt}}, ProfileID: plan.InstallProfile, ProfilePath: "/etc/blazn/node/profiles/existing-linux-worker-adopt.json"}, nodeIdentity, planSigner
}

func trustedBootstrapProfile(plan client.NodeInstallPlan) client.NodeTrustedInstallProfile {
	return client.NodeTrustedInstallProfile{ID: plan.InstallProfile, ControlPlaneOrigin: "https://control.example.test", AllowedClusterOrigins: []string{"https://cluster.example.test"}, AllowedDownloadOrigins: []string{"https://download.example.test"}, AllowedRegistryOrigins: []string{"https://registry.example.test"}, AllowedMutationRoots: []string{"/usr/local/bin", "/etc/systemd/system", "/var/lib/blazn-node-root/install-backups"}, CurrentBinaryVersion: "1.0", CurrentBinarySHA256: testHash, EmbeddedComponentSHA256: map[string]string{"service-definition": testHash}, VerifyNoSymlinkTraversal: func(string) error { return nil }}
}
func mustFingerprint(t *testing.T, identity Identity) string {
	t.Helper()
	value, err := identity.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func installPlan() client.NodeInstallPlan {
	return client.NodeInstallPlan{SchemaVersion: client.NodeSchemaVersion, PlanID: "11111111-1111-4111-8111-111111111111", NodeID: "22222222-2222-4222-8222-222222222222", EnrollmentID: "33333333-3333-4333-8333-333333333333", WorkspaceID: "44444444-4444-8444-444444444444", Digest: "sha256:" + testHash, Cluster: client.NodeInstallCluster{ID: "cluster-a"}, Target: client.NodeInstallTarget{Platform: client.NodePlatformLinux, Architecture: client.NodeArchAMD64}, Components: []client.NodeInstallComponent{{Name: "blazn", ArtifactType: "binary", SourceClass: "current_binary", SHA256: testHash}}, NodeService: client.NodeInstallService{Manager: "systemd", UnitName: "blazn-node.service", BinaryPath: "/usr/local/bin/blazn", DefinitionSHA256: testHash}, Mutations: []client.NodeInstallMutation{{Ordinal: 1, Kind: "group", Action: "create", Target: "blazn-node", Desired: map[string]any{"name": "blazn-node", "gid": float64(991), "system": true}, DesiredDigest: "sha256:" + testHash}, {Ordinal: 2, Kind: "user", Action: "create", Target: "blazn-node", Desired: map[string]any{"name": "blazn-node"}, DesiredDigest: "sha256:" + testHash}}, Rollback: client.NodeInstallRollback{BackupRootClass: "linux_node_root", BackupRoot: "/var/lib/blazn-node-root/install-backups/test"}, IssuedAt: "2026-08-22T12:00:00Z", ExpiresAt: "2026-08-22T12:15:00Z"}
}
func testCapability() client.NodeCapability {
	return client.NodeCapability{Version: 1, Host: client.NodeHostCapacity{Platform: client.NodePlatformLinux, Architecture: client.NodeArchAMD64, CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 10 << 30, Accelerators: []client.NodeAccelerator{}, Health: client.NodeCapabilityHealth{Status: "healthy", ReasonCodes: []string{}}}, Worker: client.NodeWorkerCapacity{Platform: client.NodePlatformLinux, Architecture: client.NodeArchAMD64, AllocatableCPUMillis: 900, AllocatableMemoryBytes: 1 << 30, AllocatableDiskBytes: 10 << 30, Labels: map[string]string{}, Limits: client.NodeCapabilityLimits{MaxConcurrentSandboxes: 1, MaxConcurrentAgents: 1}, Health: client.NodeCapabilityHealth{Status: "healthy", ReasonCodes: []string{}}, KubernetesBinding: client.KubernetesBinding{ClusterID: "cluster-a", NodeName: "node-a", NodeUID: "uid-a", ResourceVersion: "1"}}, SandboxBackends: []string{}, RuntimeClasses: []string{}, LocalModels: []client.LocalModelCapability{}}
}

package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingJammin/blazn/internal/client"
)

const testHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestFileIdentityStoreCreatesPrivateStableKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "identity.json")
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
	root := t.TempDir()
	binary := filepath.Join(root, "blazn")
	profilePath := filepath.Join(root, "profile.json")
	if err := os.WriteFile(binary, []byte("binary"), 0700); err != nil {
		t.Fatal(err)
	}
	profile := `{"schemaVersion":1,"id":"ubuntu-26.04-amd64-worker/v1","allowedClusterOrigins":["https://cluster.example.test"],"allowedDownloadOrigins":[],"allowedDownloadHostSuffixes":[],"allowedRegistryOrigins":[],"allowedMutationRoots":["/var/lib/blazn"],"embeddedComponentSha256":{}}`
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
	installer.now = func() time.Time { return time.Date(2026, 8, 22, 12, 2, 0, 0, time.UTC) }
	receipt, err := installer.Recover(context.Background(), plan, meta, identity)
	if err != nil || receipt.State != "active" || len(platform.rolledBack) != 0 || state.hasWAL {
		t.Fatalf("receipt=%#v rollback=%v wal=%v err=%v", receipt, platform.rolledBack, state.hasWAL, err)
	}
}

func TestFileWALCreateIsExclusive(t *testing.T) {
	store := FileStateStore{Root: filepath.Join(t.TempDir(), "state")}
	wal := InstallWAL{SchemaVersion: 1, PlanID: "plan-a"}
	if err := store.CreateWAL(wal); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateWAL(wal); err == nil {
		t.Fatal("concurrent WAL owner replaced")
	}
}

func TestInstallLockAndPrivateStateRejectConcurrentOrLinkedOwners(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store := FileStateStore{Root: root}
	release, err := store.AcquireInstallLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
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
}

func TestEnrollmentPinIsCreateOnceAndRejectsTrustReplacement(t *testing.T) {
	store := FileStateStore{Root: filepath.Join(t.TempDir(), "state")}
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
	state := &memoryState{runtime: RuntimeState{SchemaVersion: 1, Pin: EnrollmentPin{SchemaVersion: 1}, Exchange: client.ExchangeNodeEnrollmentResponse{Plan: plan, Identity: client.NodeEnrollmentIdentity{Generation: 2, SigningKeyID: "node-identity/v2", PublicKeyFingerprint: mustFingerprint(t, identity), IssuedAt: plan.IssuedAt, ExpiresAt: plan.ExpiresAt}}}}
	api := &mockAPI{}
	daemon := NewDaemon(api, state, fixedIdentity{identity}, fixedCapability{capability: testCapability()})
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
	lastProof     string
	lastHeartbeat client.NodeHeartbeat
	secret        client.NodeEnrollmentSecret
	exchange      client.ExchangeNodeEnrollmentResponse
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
	failAt     int
	applyCalls int
	rolledBack []int64
}

func (*mockPlatform) Preflight(context.Context, client.NodeInstallPlan) error { return nil }
func (*mockPlatform) ServiceState(context.Context, client.NodeInstallService) (ServicePriorState, error) {
	return ServicePriorState{}, nil
}
func (*mockPlatform) Capture(_ context.Context, m client.NodeInstallMutation, _ string) (PriorState, error) {
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
	return nil
}
func (*mockPlatform) Verify(context.Context, client.NodeInstallPlan) error { return nil }

func testIdentity(t *testing.T) Identity {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return Identity{PrivateKey: privateKey, PublicKey: publicKey}
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
	return client.NodeInstallPlan{SchemaVersion: client.NodeSchemaVersion, PlanID: "11111111-1111-4111-8111-111111111111", NodeID: "22222222-2222-4222-8222-222222222222", EnrollmentID: "33333333-3333-4333-8333-333333333333", WorkspaceID: "44444444-4444-8444-444444444444", Digest: "sha256:" + testHash, Cluster: client.NodeInstallCluster{ID: "cluster-a"}, Target: client.NodeInstallTarget{Platform: client.NodePlatformLinux, Architecture: client.NodeArchAMD64}, Components: []client.NodeInstallComponent{{Name: "blazn", ArtifactType: "binary", SourceClass: "current_binary", SHA256: testHash}}, NodeService: client.NodeInstallService{Manager: "systemd", UnitName: "blazn-node.service", BinaryPath: "/usr/local/bin/blazn", DefinitionSHA256: testHash}, Mutations: []client.NodeInstallMutation{{Ordinal: 1, Kind: "group", Action: "create", Target: "blazn-node", Desired: map[string]any{"name": "blazn-node", "gid": float64(991), "system": true}, DesiredDigest: "sha256:" + testHash}, {Ordinal: 2, Kind: "user", Action: "create", Target: "blazn-node", Desired: map[string]any{"name": "blazn-node"}, DesiredDigest: "sha256:" + testHash}}, Rollback: client.NodeInstallRollback{BackupRootClass: "linux_var_lib", BackupRoot: "/var/lib/blazn/install-backups/test"}, IssuedAt: "2026-08-22T12:00:00Z", ExpiresAt: "2026-08-22T12:15:00Z"}
}
func testCapability() client.NodeCapability {
	return client.NodeCapability{Version: 1, Host: client.NodeHostCapacity{Platform: client.NodePlatformLinux, Architecture: client.NodeArchAMD64, CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 10 << 30, Accelerators: []client.NodeAccelerator{}, Health: client.NodeCapabilityHealth{Status: "healthy", ReasonCodes: []string{}}}, Worker: client.NodeWorkerCapacity{Platform: client.NodePlatformLinux, Architecture: client.NodeArchAMD64, AllocatableCPUMillis: 900, AllocatableMemoryBytes: 1 << 30, AllocatableDiskBytes: 10 << 30, Labels: map[string]string{}, Limits: client.NodeCapabilityLimits{MaxConcurrentSandboxes: 1, MaxConcurrentAgents: 1}, Health: client.NodeCapabilityHealth{Status: "healthy", ReasonCodes: []string{}}, KubernetesBinding: client.KubernetesBinding{ClusterID: "cluster-a", NodeName: "node-a", NodeUID: "uid-a", ResourceVersion: "1"}}, SandboxBackends: []string{}, RuntimeClasses: []string{}, LocalModels: []client.LocalModelCapability{}}
}

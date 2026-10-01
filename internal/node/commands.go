package node

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/blazncloud/blazn/internal/client"
)

type CommandEnrollOptions struct {
	WorkspaceID        string                    `json:"workspaceId"`
	RequestID          string                    `json:"requestId"`
	Name               string                    `json:"name"`
	Mode               client.NodeEnrollmentMode `json:"mode"`
	MachineFingerprint string                    `json:"machineFingerprint"`
	ProfileFile        string                    `json:"profileFile"`
	KubernetesBinding  *client.KubernetesBinding `json:"kubernetesBinding,omitempty"`
}

type CommandRuntime struct {
	Service            *Service
	Installer          *Installer
	Daemon             *Daemon
	State              StateStore
	InstallerState     StateStore
	Identities         IdentityStore
	AccessToken        string
	CurrentBinaryPath  string
	CurrentVersion     string
	TrustedProfileRoot string
	PlatformFactory    func(client.NodeTrustedInstallProfile) (Platform, error)
	PrepareState       func(context.Context) error
	// RestoreState returns the daemon's private state to the ownership
	// PrepareState recorded, after a management command fails.
	RestoreState   func(context.Context) error
	CleanupClient  PrivilegedClient
	stateHandedOff bool
}

// prepareState hands the daemon's private state to the caller at most once
// per command, so the root helper records the daemon-owned ownership rather
// than its own earlier handoff.
func (c *CommandRuntime) prepareState(ctx context.Context) error {
	if c.PrepareState == nil || c.stateHandedOff {
		return nil
	}
	if err := c.PrepareState(ctx); err != nil {
		return err
	}
	c.stateHandedOff = true
	return nil
}

// restoreAfterFailure hands the private state back to the daemon when a
// management command fails after preparing it. A removed receipt means the
// daemon and its account are gone, so the state stays with the caller to
// finish cleanup.
func (c *CommandRuntime) restoreAfterFailure(ctx context.Context, receipt client.NodeInstallReceipt, err error) error {
	if err == nil || !c.stateHandedOff || c.RestoreState == nil || receipt.State == "removed" {
		return err
	}
	if restoreErr := c.RestoreState(ctx); restoreErr != nil {
		return errors.Join(err, restoreErr)
	}
	c.stateHandedOff = false
	return err
}

type managementAPI interface {
	ListNodes(context.Context, string, string) (client.NodeList, error)
	GetNode(context.Context, string, string) (client.Node, error)
}

func (c *CommandRuntime) List(ctx context.Context, workspaceID string) (client.NodeList, error) {
	if c.Service == nil || c.Service.api == nil || c.AccessToken == "" || workspaceID == "" {
		return client.NodeList{}, errors.New("node list dependencies are unavailable")
	}
	api, ok := c.Service.api.(managementAPI)
	if !ok {
		return client.NodeList{}, errors.New("node management API is unavailable")
	}
	return api.ListNodes(ctx, c.AccessToken, workspaceID)
}

func (c *CommandRuntime) Get(ctx context.Context, nodeID string) (client.Node, error) {
	if c.Service == nil || c.Service.api == nil || c.AccessToken == "" || nodeID == "" {
		return client.Node{}, errors.New("node get dependencies are unavailable")
	}
	api, ok := c.Service.api.(managementAPI)
	if !ok {
		return client.Node{}, errors.New("node management API is unavailable")
	}
	return api.GetNode(ctx, c.AccessToken, nodeID)
}

type operationAPI interface {
	CreateNodeOperation(context.Context, string, string, string, client.CreateNodeOperationRequest) (client.NodeOperation, error)
}

// Operate requests a parameterless Node operation (pause, quarantine or
// resume) against the Node's current version. The control plane completes
// these itself and returns the signed receipt.
func (c *CommandRuntime) Operate(ctx context.Context, nodeID string, operation client.NodeOperationType, requestID string) (client.NodeOperation, error) {
	if operation != "pause" && operation != "quarantine" && operation != "resume" {
		return client.NodeOperation{}, fmt.Errorf("node operation %q is not supported", operation)
	}
	node, err := c.Get(ctx, nodeID)
	if err != nil {
		return client.NodeOperation{}, err
	}
	api, ok := c.Service.api.(operationAPI)
	if !ok {
		return client.NodeOperation{}, errors.New("node operation API is unavailable")
	}
	return api.CreateNodeOperation(ctx, c.AccessToken, nodeID, requestID, client.CreateNodeOperationRequest{Type: operation, ExpectedVersion: node.Version, Parameters: []byte("{}")})
}

func (c *CommandRuntime) Enroll(ctx context.Context, options CommandEnrollOptions) (EnrollResult, error) {
	if c.Service == nil {
		return EnrollResult{}, errors.New("node enrollment service is unavailable")
	}
	if err := c.prepareState(ctx); err != nil {
		return EnrollResult{}, err
	}
	profileRoot := c.TrustedProfileRoot
	if profileRoot == "" {
		paths, err := HostProductionNodePaths()
		if err != nil {
			return EnrollResult{}, err
		}
		profileRoot = paths.ProfileRoot
	}
	cleanProfile := filepath.Clean(options.ProfileFile)
	if !filepath.IsAbs(profileRoot) || filepath.Clean(profileRoot) != profileRoot || filepath.Dir(cleanProfile) != profileRoot {
		return EnrollResult{}, errors.New("trusted profile must be one direct file under the approved profile root")
	}
	platform, architecture, err := DefaultPlatform()
	if err != nil {
		return EnrollResult{}, err
	}
	profile, err := LoadTrustedProfile(cleanProfile, c.CurrentBinaryPath, c.CurrentVersion)
	if err != nil {
		return EnrollResult{}, err
	}
	if c.PlatformFactory != nil {
		platformAdapter, err := c.PlatformFactory(profile)
		if err != nil {
			return EnrollResult{}, err
		}
		installerState := c.InstallerState
		if installerState == nil {
			installerState = c.State
		}
		c.Installer = NewInstaller(platformAdapter, installerState)
		if _, ok := installerState.(*PrivilegedInstallState); ok {
			c.Installer.uid = func() int64 { return 0 }
		}
		c.Service.installer = c.Installer
	}
	if options.Mode == client.NodeModeAdopt && (options.KubernetesBinding == nil || options.KubernetesBinding.ClusterID == "" || options.KubernetesBinding.NodeName != options.Name || options.KubernetesBinding.NodeUID == "" || options.KubernetesBinding.ResourceVersion == "") {
		return EnrollResult{}, errors.New("adopt mode requires the exact Kubernetes cluster, node name, UID, and resourceVersion")
	}
	if options.Mode == client.NodeModeFresh && options.KubernetesBinding != nil {
		return EnrollResult{}, errors.New("fresh mode cannot carry an existing Kubernetes binding")
	}
	return c.Service.Enroll(ctx, EnrollOptions{AccessToken: c.AccessToken, WorkspaceID: options.WorkspaceID, IdempotencyKey: options.RequestID, Name: options.Name, Mode: options.Mode, Platform: platform, Architecture: architecture, MachineFingerprint: options.MachineFingerprint, KubernetesBinding: options.KubernetesBinding, Profile: profile, ProfilePath: options.ProfileFile}, true)
}
func (c *CommandRuntime) Recover(ctx context.Context) (receipt client.NodeInstallReceipt, err error) {
	if c.State == nil || c.Identities == nil {
		return client.NodeInstallReceipt{}, errors.New("node recovery dependencies are unavailable")
	}
	if err := c.prepareState(ctx); err != nil {
		return client.NodeInstallReceipt{}, err
	}
	defer func() { err = c.restoreAfterFailure(ctx, receipt, err) }()
	if receipt, ok, err := c.resumePendingUninstallCleanupPrepared(ctx); ok || err != nil {
		return receipt, err
	}
	state, err := c.State.LoadRuntime()
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	identity, err := c.Identities.LoadOrCreate()
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	profile, err := LoadTrustedProfile(state.Pin.ProfilePath, c.CurrentBinaryPath, c.CurrentVersion)
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	issuedAt, err := time.Parse(time.RFC3339, state.Exchange.Plan.IssuedAt)
	if err != nil {
		return client.NodeInstallReceipt{}, errors.New("persisted plan issuedAt is invalid")
	}
	if err := verifyExchange(state.Exchange, state.Pin, identity, EnrollOptions{Platform: state.Exchange.Plan.Target.Platform, Architecture: state.Exchange.Plan.Target.Architecture, Profile: profile}, issuedAt); err != nil {
		return client.NodeInstallReceipt{}, fmt.Errorf("reverify signed plan before recovery: %w", err)
	}
	if c.PlatformFactory != nil {
		platformAdapter, factoryErr := c.PlatformFactory(profile)
		if factoryErr != nil {
			return client.NodeInstallReceipt{}, factoryErr
		}
		installerState := c.InstallerState
		if installerState == nil {
			installerState = c.State
		}
		c.Installer = NewInstaller(platformAdapter, installerState)
	}
	if c.Installer == nil {
		return client.NodeInstallReceipt{}, errors.New("node recovery installer is unavailable")
	}
	receipt, err = c.Installer.Recover(ctx, state.Exchange.Plan, state.Exchange.Identity, identity)
	if err == nil && receipt.State == "removed" {
		err = c.beginAndResumeUninstallCleanup(ctx, state.Exchange.Plan, receipt)
	}
	return receipt, err
}

func (c *CommandRuntime) Repair(ctx context.Context) (receipt client.NodeInstallReceipt, err error) {
	defer func() { err = c.restoreAfterFailure(ctx, receipt, err) }()
	state, identity, profile, err := c.lifecycleContext(ctx, true)
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	if err := c.configureInstaller(profile); err != nil {
		return client.NodeInstallReceipt{}, err
	}
	receipt, err = c.Installer.Repair(ctx, state.Exchange.Plan, state.Exchange.Identity, identity)
	if err == nil {
		err = c.Installer.FinalizeServiceState(ctx, state.Exchange.Plan)
	}
	return receipt, err
}

func (c *CommandRuntime) Uninstall(ctx context.Context, removeManagedRuntime bool) (receipt client.NodeInstallReceipt, err error) {
	defer func() { err = c.restoreAfterFailure(ctx, receipt, err) }()
	if receipt, ok, err := c.resumePendingUninstallCleanup(ctx); ok || err != nil {
		return receipt, err
	}
	state, identity, profile, err := c.lifecycleContext(ctx, false)
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	if err := c.configureInstaller(profile); err != nil {
		return client.NodeInstallReceipt{}, err
	}
	c.Installer.SetDrainer(func(ctx context.Context) error { return c.drainNode(ctx, state, identity) })
	c.Installer.SetRebootstrapper(func(ctx context.Context) error { return c.rebootstrapNode(ctx, state, identity) })
	receipt, err = c.Installer.Uninstall(ctx, state.Exchange.Plan, state.Exchange.Identity, identity, removeManagedRuntime)
	if err != nil {
		return receipt, err
	}
	if receipt.State == "removed" {
		err = c.beginAndResumeUninstallCleanup(ctx, state.Exchange.Plan, receipt)
	}
	return receipt, err
}

func (c *CommandRuntime) beginAndResumeUninstallCleanup(ctx context.Context, plan client.NodeInstallPlan, receipt client.NodeInstallReceipt) error {
	store, ok := c.State.(FileStateStore)
	if !ok {
		return errors.New("node cleanup requires file-backed service state")
	}
	journal := UninstallCleanupJournal{SchemaVersion: 1, Plan: plan, Receipt: receipt, CreatedAt: nowString(time.Now())}
	if err := store.CreateUninstallCleanup(journal); err != nil {
		return err
	}
	return c.resumeUninstallCleanup(ctx, store, journal)
}

type drainAPI interface {
	DrainNode(context.Context, string, client.NodeDrainRequest) (client.NodeDrainResponse, error)
}

// drainNode asks the control plane to mark this node's own Kubernetes Node
// retired and unschedulable, proven by the node identity.
func (c *CommandRuntime) drainNode(ctx context.Context, state RuntimeState, identity Identity) error {
	if c.Service == nil || state.KubernetesBinding == nil {
		return errors.New("node drain requires an active Kubernetes binding")
	}
	api, ok := c.Service.api.(drainAPI)
	if !ok {
		return errors.New("node drain API is unavailable")
	}
	request := client.NodeDrainRequest{NodeID: state.Exchange.Plan.NodeID, IdentityGeneration: state.Exchange.Identity.Generation,
		SentAt: time.Now().UTC().Format(time.RFC3339), KubernetesBinding: *state.KubernetesBinding}
	proof, err := nodeProof(identity.PrivateKey, "blazn-node-drain-v1", request)
	if err != nil {
		return err
	}
	_, err = api.DrainNode(ctx, proof, request)
	return err
}

type rebootstrapAPI interface {
	RebootstrapNode(context.Context, string, client.NodeRebootstrapRequest) (client.NodeRebootstrapResponse, error)
}

// rebootstrapNode asks the control plane to return this node's own
// Kubernetes Node to bootstrap quarantine, proven by the node identity.
func (c *CommandRuntime) rebootstrapNode(ctx context.Context, state RuntimeState, identity Identity) error {
	if c.Service == nil || state.KubernetesBinding == nil {
		return errors.New("node rebootstrap requires an active Kubernetes binding")
	}
	api, ok := c.Service.api.(rebootstrapAPI)
	if !ok {
		return errors.New("node rebootstrap API is unavailable")
	}
	request := client.NodeRebootstrapRequest{NodeID: state.Exchange.Plan.NodeID, IdentityGeneration: state.Exchange.Identity.Generation,
		SentAt: time.Now().UTC().Format(time.RFC3339), KubernetesBinding: *state.KubernetesBinding}
	proof, err := nodeProof(identity.PrivateKey, "blazn-node-rebootstrap-v1", request)
	if err != nil {
		return err
	}
	_, err = api.RebootstrapNode(ctx, proof, request)
	return err
}

type retirementAPI interface {
	RetireNode(context.Context, string, string, client.NodeRetirementRequest) (client.Node, error)
}

// retireRemovedNode tells the control plane that the node identity has
// uninstalled itself, while that identity still exists to sign the request.
// A transport or server failure keeps the cleanup journal so rerunning
// uninstall retries with the same idempotency key. A definitive rejection
// (already removed, never activated, or a receipt the server cannot chain)
// cannot be fixed by retrying, so local cleanup continues.
func (c *CommandRuntime) retireRemovedNode(ctx context.Context, receipt client.NodeInstallReceipt) error {
	if c.Service == nil || c.Identities == nil {
		return nil
	}
	api, ok := c.Service.api.(retirementAPI)
	if !ok {
		return errors.New("node retirement API is unavailable")
	}
	identity, err := c.Identities.LoadOrCreate()
	if err != nil {
		return fmt.Errorf("load node identity for retirement: %w", err)
	}
	request := client.NodeRetirementRequest{Receipt: receipt}
	proof, err := nodeProof(identity.PrivateKey, "blazn-node-retirement-v1", request)
	if err != nil {
		return err
	}
	retired, err := api.RetireNode(ctx, proof, "node-retire-"+receipt.ReceiptID, request)
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && (apiErr.Body.Code == "identity_rejected" || apiErr.Body.Code == "state_conflict") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("retire node with the control plane (rerun uninstall to retry): %w", err)
	}
	if retired.ID != receipt.NodeID || retired.LifecycleState != "removed" {
		return errors.New("retirement response differs from the removed node")
	}
	return nil
}

func (c *CommandRuntime) resumePendingUninstallCleanup(ctx context.Context) (client.NodeInstallReceipt, bool, error) {
	if err := c.prepareState(ctx); err != nil {
		return client.NodeInstallReceipt{}, true, err
	}
	return c.resumePendingUninstallCleanupPrepared(ctx)
}

func (c *CommandRuntime) resumePendingUninstallCleanupPrepared(ctx context.Context) (client.NodeInstallReceipt, bool, error) {
	store, ok := c.State.(FileStateStore)
	if !ok {
		return client.NodeInstallReceipt{}, false, nil
	}
	journal, err := store.LoadUninstallCleanup()
	if errors.Is(err, os.ErrNotExist) {
		return client.NodeInstallReceipt{}, false, nil
	}
	if err != nil {
		return client.NodeInstallReceipt{}, true, err
	}
	return journal.Receipt, true, c.resumeUninstallCleanup(ctx, store, journal)
}
func (c *CommandRuntime) resumeUninstallCleanup(ctx context.Context, store FileStateStore, journal UninstallCleanupJournal) error {
	platform := "linux"
	if journal.Plan.Target.Platform == client.NodePlatformMacOS {
		platform = "macos"
	}
	cleanupClient := c.CleanupClient
	if cleanupClient == nil {
		cleanupClient = PipePrivilegedClient{HelperPath: DefaultRootHelperPath, UseSudo: currentUID() != 0, Timeout: 2 * time.Minute}
	}
	privileged := &PrivilegedInstallState{Client: cleanupClient, Local: store, Platform: platform}
	privileged.BindPlan(journal.Plan)
	privileged.BindContext(ctx)
	defer privileged.BindContext(nil)
	wal, err := privileged.LoadWAL()
	if errors.Is(err, os.ErrNotExist) {
		receipt, receiptErr := privileged.LoadReceipt()
		if receiptErr == nil && sameJSON(receipt, journal.Receipt) {
			return store.RemoveUninstallCleanup()
		}
		return errors.New("uninstall cleanup lost its root WAL")
	}
	if err != nil {
		return err
	}
	if wal.TerminalReceipt == nil || !sameJSON(*wal.TerminalReceipt, journal.Receipt) || cleanupCheckpointRank(wal.Checkpoint) < 1 {
		return errors.New("uninstall cleanup journal differs from root WAL")
	}
	if cleanupCheckpointRank(wal.Checkpoint) < 2 {
		request := RootRequest{SchemaVersion: RootHelperSchema, Operation: RootRemoveSupport, Platform: platform, Plan: journal.Plan}
		if _, err := privileged.Client.Call(ctx, request); err != nil {
			return err
		}
		wal.Checkpoint = "cleanup_support_removed"
		wal.UpdatedAt = nowString(time.Now())
		if err := privileged.SaveWAL(wal); err != nil {
			return err
		}
	}
	if cleanupCheckpointRank(wal.Checkpoint) < 3 {
		if err := c.retireRemovedNode(ctx, journal.Receipt); err != nil {
			return err
		}
		if err := removeLocalNodeState(store); err != nil {
			return err
		}
		wal.Checkpoint = "cleanup_local_state_removed"
		wal.UpdatedAt = nowString(time.Now())
		if err := privileged.SaveWAL(wal); err != nil {
			return err
		}
	}
	if err := privileged.SaveReceipt(journal.Receipt); err != nil {
		return err
	}
	if err := privileged.RemoveWAL(); err != nil {
		return err
	}
	return store.RemoveUninstallCleanup()
}
func cleanupCheckpointRank(value string) int {
	switch value {
	case "cleanup_pending":
		return 1
	case "cleanup_support_removed":
		return 2
	case "cleanup_local_state_removed":
		return 3
	}
	return 0
}
func removeLocalNodeState(store FileStateStore) error {
	for _, name := range []string{"identity.json", "runtime.json", "enrollment-pin.json"} {
		path := filepath.Join(store.Root, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("node service-state cleanup encountered an unsafe entry")
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

func (c *CommandRuntime) lifecycleContext(ctx context.Context, requireCurrent bool) (RuntimeState, Identity, client.NodeTrustedInstallProfile, error) {
	if c.State == nil || c.Identities == nil {
		return RuntimeState{}, Identity{}, client.NodeTrustedInstallProfile{}, errors.New("node lifecycle dependencies are unavailable")
	}
	if err := c.prepareState(ctx); err != nil {
		return RuntimeState{}, Identity{}, client.NodeTrustedInstallProfile{}, err
	}
	state, err := c.State.LoadRuntime()
	if err != nil {
		return RuntimeState{}, Identity{}, client.NodeTrustedInstallProfile{}, err
	}
	identity, err := c.Identities.LoadOrCreate()
	if err != nil {
		return RuntimeState{}, Identity{}, client.NodeTrustedInstallProfile{}, err
	}
	profile, err := LoadTrustedProfile(state.Pin.ProfilePath, c.CurrentBinaryPath, c.CurrentVersion)
	if err != nil {
		return RuntimeState{}, Identity{}, client.NodeTrustedInstallProfile{}, err
	}
	when := time.Now()
	if !requireCurrent {
		issuedAt, parseErr := time.Parse(time.RFC3339, state.Exchange.Plan.IssuedAt)
		if parseErr != nil {
			return RuntimeState{}, Identity{}, client.NodeTrustedInstallProfile{}, parseErr
		}
		when = issuedAt
	}
	if err := verifyExchange(state.Exchange, state.Pin, identity, EnrollOptions{Platform: state.Exchange.Plan.Target.Platform, Architecture: state.Exchange.Plan.Target.Architecture, Profile: profile}, when); err != nil {
		if requireCurrent {
			return RuntimeState{}, Identity{}, client.NodeTrustedInstallProfile{}, fmt.Errorf("repair requires an authorized fresh, unexpired plan: %w", err)
		}
		return RuntimeState{}, Identity{}, client.NodeTrustedInstallProfile{}, err
	}
	return state, identity, profile, nil
}

func (c *CommandRuntime) configureInstaller(profile client.NodeTrustedInstallProfile) error {
	if c.PlatformFactory != nil {
		platform, err := c.PlatformFactory(profile)
		if err != nil {
			return err
		}
		state := c.InstallerState
		if state == nil {
			state = c.State
		}
		c.Installer = NewInstaller(platform, state)
		if _, ok := state.(*PrivilegedInstallState); ok {
			c.Installer.uid = func() int64 { return 0 }
		}
	}
	if c.Installer == nil {
		return errors.New("node lifecycle installer is unavailable")
	}
	return nil
}

func NewProductionCommandRuntime(api API, accessToken, currentVersion string, join JoinCoordinator, capabilities CapabilityProvider, embedded map[string][]byte) (*CommandRuntime, error) {
	if api == nil || accessToken == "" || currentVersion == "" {
		return nil, errors.New("production node runtime dependencies are incomplete")
	}
	binary, err := os.Executable()
	if err != nil {
		return nil, err
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return nil, err
	}
	paths, err := HostProductionNodePaths()
	if err != nil {
		return nil, err
	}
	return newProductionCommandRuntime(api, accessToken, currentVersion, join, capabilities, embedded, paths, binary)
}

// NewProductionDaemonCommandRuntime uses only the finalized service-owned
// runtime, node identity, and pinned control-plane origin. In particular it
// never opens a human workspace session or accepts a user access token.
func NewProductionDaemonCommandRuntime(currentVersion string, httpClient *http.Client, capabilities CapabilityProvider) (*CommandRuntime, error) {
	if currentVersion == "" {
		return nil, errors.New("production node daemon version is unavailable")
	}
	paths, err := HostProductionNodePaths()
	if err != nil {
		return nil, err
	}
	return newProductionDaemonCommandRuntime(paths, currentVersion, httpClient, capabilities)
}

func newProductionDaemonCommandRuntime(paths ProductionNodePaths, currentVersion string, httpClient *http.Client, capabilities CapabilityProvider) (*CommandRuntime, error) {
	if paths.ServiceStateRoot == "" || paths.RootStateRoot == "" || paths.ServiceStateRoot == paths.RootStateRoot {
		return nil, errors.New("production node daemon paths are invalid")
	}
	platformName := "linux"
	if paths.RootStateRoot == MacOSNodeRootStateRoot {
		platformName = "macos"
	}
	state := FileStateStore{Root: paths.ServiceStateRoot}
	persisted, err := state.LoadRuntime()
	if err != nil {
		return nil, fmt.Errorf("load daemon service state: %w", err)
	}
	if !validControlPlaneOrigin(persisted.ControlPlaneOrigin) {
		return nil, errors.New("persisted daemon control-plane origin is invalid")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	api, err := client.New(persisted.ControlPlaneOrigin, httpClient)
	if err != nil {
		return nil, err
	}
	identities := FileIdentityStore{Path: filepath.Join(paths.ServiceStateRoot, "identity.json")}
	if capabilities == nil {
		capabilities = ProductionCapabilityProvider{State: state, Observer: PrivilegedLiveNodeObserver{Client: PipeObservationClient{HelperPath: DefaultRootHelperPath, Timeout: 30 * time.Second}, Platform: platformName}}
	}
	return &CommandRuntime{Daemon: NewDaemon(api, state, identities, capabilities), State: state, Identities: identities, CurrentVersion: currentVersion}, nil
}

func newProductionCommandRuntime(api API, accessToken, currentVersion string, join JoinCoordinator, capabilities CapabilityProvider, embedded map[string][]byte, paths ProductionNodePaths, binary string) (*CommandRuntime, error) {
	if api == nil || accessToken == "" || currentVersion == "" || !filepath.IsAbs(binary) || paths.ServiceStateRoot == "" || paths.RootStateRoot == "" || paths.ServiceStateRoot == paths.RootStateRoot || paths.ProfileRoot == "" {
		return nil, errors.New("production node runtime dependencies or paths are invalid")
	}
	state := FileStateStore{Root: paths.ServiceStateRoot}
	platformName := "linux"
	if paths.RootStateRoot == MacOSNodeRootStateRoot {
		platformName = "macos"
	}
	privileged := PipePrivilegedClient{HelperPath: DefaultRootHelperPath, UseSudo: currentUID() != 0, Timeout: 2 * time.Minute}
	installerState := &PrivilegedInstallState{Client: privileged, Local: state, Platform: platformName}
	identities := FileIdentityStore{Path: filepath.Join(paths.ServiceStateRoot, "identity.json")}
	if capabilities == nil {
		capabilities = ProductionCapabilityProvider{State: state, Observer: PrivilegedLiveNodeObserver{Client: privileged, Platform: platformName}}
	}
	if join == nil {
		joinAPI, ok := api.(JoinAPI)
		if !ok {
			return nil, errors.New("production node API does not expose the frozen join credential endpoints")
		}
		coordinator, err := NewBrokerJoinCoordinator(joinAPI, state, identities)
		if err != nil {
			return nil, err
		}
		join = coordinator
	}
	service := NewService(api, identities, state, nil)
	daemon := NewDaemon(api, state, identities, capabilities)
	runtime := &CommandRuntime{Service: service, Daemon: daemon, State: state, InstallerState: installerState, Identities: identities, AccessToken: accessToken, CurrentBinaryPath: binary, CurrentVersion: currentVersion, TrustedProfileRoot: paths.ProfileRoot}
	runtime.PrepareState = func(ctx context.Context) error {
		return prepareProductionServiceState(ctx, paths.ServiceStateRoot, binary)
	}
	runtime.RestoreState = func(ctx context.Context) error {
		return restoreProductionServiceState(ctx, paths.ServiceStateRoot, binary)
	}
	runtime.PlatformFactory = func(profile client.NodeTrustedInstallProfile) (Platform, error) {
		resolver := TrustedMaterialResolver{Profile: profile, CurrentBinaryPath: binary, Embedded: embedded, HTTP: &http.Client{Timeout: 2 * time.Minute}, MaxBytes: 512 << 20}
		return SupportedPlatformAdapter(privileged, resolver, join)
	}
	return runtime, nil
}
func (c *CommandRuntime) Heartbeat(ctx context.Context) (HeartbeatResult, error) {
	if c.Daemon == nil {
		return HeartbeatResult{}, errors.New("node daemon is unavailable")
	}
	return c.Daemon.Heartbeat(ctx)
}

func (c *CommandRuntime) Serve(ctx context.Context, interval time.Duration) error {
	if interval < time.Second || interval > 5*time.Minute {
		return errors.New("node serve heartbeat interval is invalid")
	}
	if _, err := c.Heartbeat(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := c.Heartbeat(ctx); err != nil {
				return err
			}
		}
	}
}

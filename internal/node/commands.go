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
}

func (c *CommandRuntime) Enroll(ctx context.Context, options CommandEnrollOptions) (EnrollResult, error) {
	if c.Service == nil {
		return EnrollResult{}, errors.New("node enrollment service is unavailable")
	}
	if c.PrepareState != nil {
		if err := c.PrepareState(ctx); err != nil {
			return EnrollResult{}, err
		}
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
func (c *CommandRuntime) Recover(ctx context.Context) (client.NodeInstallReceipt, error) {
	if c.State == nil || c.Identities == nil {
		return client.NodeInstallReceipt{}, errors.New("node recovery dependencies are unavailable")
	}
	if c.PrepareState != nil {
		if err := c.PrepareState(ctx); err != nil {
			return client.NodeInstallReceipt{}, err
		}
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
	receipt, err := c.Installer.Recover(ctx, state.Exchange.Plan, state.Exchange.Identity, identity)
	return receipt, err
}

func (c *CommandRuntime) Repair(ctx context.Context) (client.NodeInstallReceipt, error) {
	state, identity, profile, err := c.lifecycleContext(ctx, true)
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	if err := c.configureInstaller(profile); err != nil {
		return client.NodeInstallReceipt{}, err
	}
	receipt, err := c.Installer.Repair(ctx, state.Exchange.Plan, state.Exchange.Identity, identity)
	if err == nil {
		err = c.Installer.FinalizeServiceState(ctx, state.Exchange.Plan)
	}
	return receipt, err
}

func (c *CommandRuntime) Uninstall(ctx context.Context, removeManagedRuntime bool) (client.NodeInstallReceipt, error) {
	state, identity, profile, err := c.lifecycleContext(ctx, false)
	if err != nil {
		return client.NodeInstallReceipt{}, err
	}
	if err := c.configureInstaller(profile); err != nil {
		return client.NodeInstallReceipt{}, err
	}
	receipt, err := c.Installer.Uninstall(ctx, state.Exchange.Plan, state.Exchange.Identity, identity, removeManagedRuntime)
	if err != nil {
		return receipt, err
	}
	if remover, ok := c.Installer.platform.(interface {
		RemoveServiceSupport(context.Context, client.NodeInstallPlan) error
	}); ok {
		if err := remover.RemoveServiceSupport(ctx, state.Exchange.Plan); err != nil {
			return receipt, err
		}
	}
	if store, ok := c.State.(FileStateStore); ok {
		for _, name := range []string{"identity.json", "runtime.json", "enrollment-pin.json"} {
			path := filepath.Join(store.Root, name)
			info, statErr := os.Lstat(path)
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				return receipt, errors.New("node service-state cleanup encountered an unsafe entry")
			}
			if removeErr := os.Remove(path); removeErr != nil {
				return receipt, removeErr
			}
		}
	}
	return receipt, nil
}

func (c *CommandRuntime) lifecycleContext(ctx context.Context, requireCurrent bool) (RuntimeState, Identity, client.NodeTrustedInstallProfile, error) {
	if c.State == nil || c.Identities == nil {
		return RuntimeState{}, Identity{}, client.NodeTrustedInstallProfile{}, errors.New("node lifecycle dependencies are unavailable")
	}
	if c.PrepareState != nil {
		if err := c.PrepareState(ctx); err != nil {
			return RuntimeState{}, Identity{}, client.NodeTrustedInstallProfile{}, err
		}
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

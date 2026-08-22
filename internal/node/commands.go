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
}

func (c *CommandRuntime) Enroll(ctx context.Context, options CommandEnrollOptions) (EnrollResult, error) {
	if c.Service == nil {
		return EnrollResult{}, errors.New("node enrollment service is unavailable")
	}
	if currentUID() != 0 {
		return EnrollResult{}, errors.New("node install requires a privileged root execution boundary")
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
	return c.Installer.Recover(ctx, state.Exchange.Plan, state.Exchange.Identity, identity)
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
	if binary != "/usr/local/bin/blazn" {
		return nil, errors.New("production node runtime requires the receipt-owned /usr/local/bin/blazn binary")
	}
	paths, err := HostProductionNodePaths()
	if err != nil {
		return nil, err
	}
	return newProductionCommandRuntime(api, accessToken, currentVersion, join, capabilities, embedded, paths, binary)
}

func newProductionCommandRuntime(api API, accessToken, currentVersion string, join JoinCoordinator, capabilities CapabilityProvider, embedded map[string][]byte, paths ProductionNodePaths, binary string) (*CommandRuntime, error) {
	if api == nil || accessToken == "" || currentVersion == "" || binary != defaultRootBinaryPath || paths.ServiceStateRoot == "" || paths.RootStateRoot == "" || paths.ServiceStateRoot == paths.RootStateRoot || paths.ProfileRoot == "" {
		return nil, errors.New("production node runtime dependencies or paths are invalid")
	}
	state := FileStateStore{Root: paths.ServiceStateRoot}
	installerState := FileStateStore{Root: paths.RootStateRoot}
	identities := FileIdentityStore{Path: filepath.Join(paths.ServiceStateRoot, "identity.json")}
	if capabilities == nil {
		capabilities = ProductionCapabilityProvider{State: state}
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
	runtime.PlatformFactory = func(profile client.NodeTrustedInstallProfile) (Platform, error) {
		resolver := TrustedMaterialResolver{Profile: profile, CurrentBinaryPath: binary, Embedded: embedded, HTTP: &http.Client{Timeout: 2 * time.Minute}, MaxBytes: 512 << 20}
		return SupportedPlatformAdapter(PipePrivilegedClient{HelperPath: DefaultRootHelperPath, UseSudo: currentUID() != 0, Timeout: 2 * time.Minute}, resolver, join)
	}
	return runtime, nil
}
func (c *CommandRuntime) Heartbeat(ctx context.Context) (HeartbeatResult, error) {
	if c.Daemon == nil {
		return HeartbeatResult{}, errors.New("node daemon is unavailable")
	}
	return c.Daemon.Heartbeat(ctx)
}

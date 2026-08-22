package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/KingJammin/blazn/internal/client"
)

type CommandEnrollOptions struct {
	WorkspaceID        string                    `json:"workspaceId"`
	RequestID          string                    `json:"requestId"`
	Name               string                    `json:"name"`
	Mode               client.NodeEnrollmentMode `json:"mode"`
	MachineFingerprint string                    `json:"machineFingerprint"`
	ProfileFile        string                    `json:"profileFile"`
}

type CommandRuntime struct {
	Service           *Service
	Installer         *Installer
	Daemon            *Daemon
	State             StateStore
	Identities        IdentityStore
	AccessToken       string
	CurrentBinaryPath string
	CurrentVersion    string
}

func (c *CommandRuntime) Enroll(ctx context.Context, options CommandEnrollOptions) (EnrollResult, error) {
	if c.Service == nil {
		return EnrollResult{}, errors.New("node enrollment service is unavailable")
	}
	if currentUID() != 0 {
		return EnrollResult{}, errors.New("node install requires a privileged root execution boundary")
	}
	platform, architecture, err := DefaultPlatform()
	if err != nil {
		return EnrollResult{}, err
	}
	profile, err := LoadTrustedProfile(options.ProfileFile, c.CurrentBinaryPath, c.CurrentVersion)
	if err != nil {
		return EnrollResult{}, err
	}
	return c.Service.Enroll(ctx, EnrollOptions{AccessToken: c.AccessToken, WorkspaceID: options.WorkspaceID, IdempotencyKey: options.RequestID, Name: options.Name, Mode: options.Mode, Platform: platform, Architecture: architecture, MachineFingerprint: options.MachineFingerprint, Profile: profile, ProfilePath: options.ProfileFile}, true)
}
func (c *CommandRuntime) Recover(ctx context.Context) (client.NodeInstallReceipt, error) {
	if c.Installer == nil || c.State == nil || c.Identities == nil {
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
	return c.Installer.Recover(ctx, state.Exchange.Plan, state.Exchange.Identity, identity)
}
func (c *CommandRuntime) Heartbeat(ctx context.Context) (HeartbeatResult, error) {
	if c.Daemon == nil {
		return HeartbeatResult{}, errors.New("node daemon is unavailable")
	}
	return c.Daemon.Heartbeat(ctx)
}

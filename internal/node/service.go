package node

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/blazncloud/blazn/internal/client"
)

type API interface {
	CreateNodeEnrollment(context.Context, string, string, string, client.CreateNodeEnrollmentRequest) (client.NodeEnrollmentSecret, error)
	ExchangeNodeEnrollment(context.Context, string, client.ExchangeNodeEnrollmentRequest) (client.ExchangeNodeEnrollmentResponse, error)
	SubmitNodeHeartbeat(context.Context, string, client.NodeHeartbeat) error
	ActivateNode(context.Context, string, string, client.NodeActivationRequest) (client.Node, error)
}

type EnrollOptions struct {
	AccessToken        string
	WorkspaceID        string
	IdempotencyKey     string
	Name               string
	Mode               client.NodeEnrollmentMode
	Platform           client.NodePlatform
	Architecture       client.NodeArchitecture
	MachineFingerprint string
	KubernetesBinding  *client.KubernetesBinding
	Profile            client.NodeTrustedInstallProfile
	ProfilePath        string
}

type EnrollResult struct {
	State     RuntimeState               `json:"state"`
	Installed bool                       `json:"installed"`
	Receipt   *client.NodeInstallReceipt `json:"receipt,omitempty"`
}

type Service struct {
	api        API
	identities IdentityStore
	state      StateStore
	installer  *Installer
	now        func() time.Time
}

func NewService(api API, identities IdentityStore, state StateStore, installer *Installer) *Service {
	return &Service{api: api, identities: identities, state: state, installer: installer, now: time.Now}
}

func (s *Service) Enroll(ctx context.Context, options EnrollOptions, install bool) (EnrollResult, error) {
	if s.api == nil || s.identities == nil || s.state == nil {
		return EnrollResult{}, errors.New("node service dependencies are incomplete")
	}
	if options.AccessToken == "" || options.WorkspaceID == "" || len(options.IdempotencyKey) < 8 || options.Name == "" || options.MachineFingerprint == "" || options.Profile.ID == "" {
		return EnrollResult{}, errors.New("node enrollment inputs are incomplete")
	}
	identity, err := s.identities.LoadOrCreate()
	if err != nil {
		return EnrollResult{}, fmt.Errorf("load node identity: %w", err)
	}
	secret, err := s.api.CreateNodeEnrollment(ctx, options.AccessToken, options.WorkspaceID, options.IdempotencyKey, client.CreateNodeEnrollmentRequest{Name: options.Name, Mode: options.Mode, Platform: options.Platform, Architecture: options.Architecture})
	if err != nil {
		return EnrollResult{}, err
	}
	if err := client.ValidateNodeEnrollmentSecret(secret); err != nil {
		return EnrollResult{}, fmt.Errorf("validate node enrollment response: %w", err)
	}
	pin := EnrollmentPin{SchemaVersion: 1, WorkspaceID: options.WorkspaceID, EnrollmentID: secret.ID, IdempotencyKey: options.IdempotencyKey, Hostname: options.Name, MachineFingerprint: options.MachineFingerprint, ProfileID: options.Profile.ID, ProfilePath: options.ProfilePath, PlanSigningKey: secret.PlanSigningKey, PinnedAt: nowString(s.now())}
	if err := s.state.Pin(pin); err != nil {
		return EnrollResult{}, fmt.Errorf("persist pinned plan signer before exchange: %w", err)
	}
	response, err := s.api.ExchangeNodeEnrollment(ctx, secret.ID, client.ExchangeNodeEnrollmentRequest{Token: secret.Token, MachineFingerprint: options.MachineFingerprint, NodePublicKey: identity.PublicBase64(), Platform: options.Platform, Architecture: options.Architecture, KubernetesBinding: options.KubernetesBinding})
	if err != nil {
		return EnrollResult{}, err
	}
	if err := client.ValidateExchangeNodeEnrollmentResponse(response); err != nil {
		return EnrollResult{}, fmt.Errorf("validate node enrollment exchange: %w", err)
	}
	if err := verifyExchange(response, pin, identity, options, s.now()); err != nil {
		return EnrollResult{}, err
	}
	if install {
		if s.installer == nil {
			return EnrollResult{}, errors.New("privileged installer is unavailable")
		}
		authorization := BootstrapAuthorization{EnrollmentID: secret.ID, Token: secret.Token, MachineFingerprint: options.MachineFingerprint, NodePublicKey: identity.PublicBase64(), Platform: options.Platform, Architecture: options.Architecture, KubernetesBinding: options.KubernetesBinding, PlanSigningKey: secret.PlanSigningKey, Expected: response, ProfileID: options.Profile.ID, ProfilePath: options.ProfilePath}
		if err := s.installer.AuthorizeBootstrap(ctx, authorization); err != nil {
			return EnrollResult{}, err
		}
	}
	state := RuntimeState{SchemaVersion: 1, ControlPlaneOrigin: options.Profile.ControlPlaneOrigin, Pin: pin, Exchange: response, KubernetesBinding: options.KubernetesBinding, UpdatedAt: nowString(s.now())}
	if err := s.state.SaveRuntime(state); err != nil {
		return EnrollResult{}, fmt.Errorf("persist verified node plan: %w", err)
	}
	result := EnrollResult{State: state}
	if install {
		receipt, err := s.installer.Install(ctx, response.Plan, response.Identity, identity)
		if err != nil {
			return result, err
		}
		result.Installed = true
		result.Receipt = &receipt
		if provider, ok := s.installer.platform.(interface {
			KubernetesBinding() *client.KubernetesBinding
		}); ok {
			state.KubernetesBinding = provider.KubernetesBinding()
			state.UpdatedAt = nowString(s.now())
			if state.KubernetesBinding == nil {
				return result, errors.New("installed node did not return a verified Kubernetes binding")
			}
			if err := s.state.SaveRuntime(state); err != nil {
				return result, fmt.Errorf("persist verified Kubernetes binding: %w", err)
			}
			result.State = state
		}
		if state.KubernetesBinding == nil {
			return result, errors.New("installed node has no verified Kubernetes binding for activation")
		}
		expectedVersion := int64(1)
		if response.Plan.Mode == client.NodeModeFresh {
			expectedVersion = 2
		}
		activation := client.NodeActivationRequest{ExpectedVersion: expectedVersion, Receipt: receipt, KubernetesBinding: *state.KubernetesBinding}
		proof, err := nodeProof(identity.PrivateKey, "blazn-node-activation-v1", activation)
		if err != nil {
			return result, err
		}
		activated, err := s.api.ActivateNode(ctx, proof, "node-activate-"+receipt.ReceiptID, activation)
		if err != nil {
			return result, fmt.Errorf("activate installed node: %w", err)
		}
		if err := client.ValidateNode(activated); err != nil || activated.ID != response.Plan.NodeID || activated.LifecycleState != "active" || activated.TrustState != "verified" || activated.KubernetesBinding == nil || *activated.KubernetesBinding != *state.KubernetesBinding {
			return result, errors.New("activation response differs from the installed node")
		}
		releasedBinding, err := s.installer.ReleaseNodeCapacity(ctx, response.Plan, receipt)
		if err != nil {
			return result, fmt.Errorf("release activated node capacity: %w", err)
		}
		if releasedBinding.ClusterID != state.KubernetesBinding.ClusterID || releasedBinding.NodeName != state.KubernetesBinding.NodeName || releasedBinding.NodeUID != state.KubernetesBinding.NodeUID || releasedBinding.ResourceVersion == "" {
			return result, errors.New("released capacity binding differs from the activated node")
		}
		state.KubernetesBinding = releasedBinding
		state.UpdatedAt = nowString(s.now())
		if err := s.state.SaveRuntime(state); err != nil {
			return result, fmt.Errorf("persist released Kubernetes binding: %w", err)
		}
		result.State = state
		if err := s.installer.FinalizeServiceState(ctx, response.Plan); err != nil {
			return result, fmt.Errorf("finalize daemon-owned node state: %w", err)
		}
	}
	return result, nil
}

func verifyExchange(response client.ExchangeNodeEnrollmentResponse, pin EnrollmentPin, identity Identity, options EnrollOptions, now time.Time) error {
	if err := client.ValidateExchangeNodeEnrollmentResponse(response); err != nil {
		return err
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(pin.PlanSigningKey.PublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return errors.New("pinned plan signer public key is invalid")
	}
	if response.Plan.SigningKeyID != pin.PlanSigningKey.KeyID {
		return errors.New("install plan signer differs from the pre-exchange pin")
	}
	pinnedFingerprint, err := client.NodePublicKeyFingerprint(ed25519.PublicKey(publicKey))
	if err != nil || pinnedFingerprint != pin.PlanSigningKey.Fingerprint {
		return errors.New("pinned plan signer fingerprint is inconsistent")
	}
	fingerprint, err := identity.Fingerprint()
	if err != nil || response.Identity.PublicKeyFingerprint != fingerprint {
		return errors.New("enrollment identity does not bind the local node key")
	}
	trust := client.NodeInstallPlanTrust{Now: now, Keyring: client.NodeSigningKeyring{pin.PlanSigningKey.KeyID: ed25519.PublicKey(publicKey)}, WorkspaceID: pin.WorkspaceID, EnrollmentID: pin.EnrollmentID, NodeID: response.Plan.NodeID, Hostname: pin.Hostname, MachineFingerprint: pin.MachineFingerprint, NodePublicKey: identity.PublicKey, Platform: options.Platform, Architecture: options.Architecture, IdempotencyKey: pin.IdempotencyKey, Profile: options.Profile}
	if err := client.VerifyNodeInstallPlan(response.Plan, trust); err != nil {
		return fmt.Errorf("verify signed install plan and local sources: %w", err)
	}
	return nil
}

func DefaultPlatform() (client.NodePlatform, client.NodeArchitecture, error) {
	var platform client.NodePlatform
	switch runtime.GOOS {
	case "linux":
		platform = client.NodePlatformLinux
	case "darwin":
		platform = client.NodePlatformMacOS
	default:
		return "", "", fmt.Errorf("unsupported node platform %s", runtime.GOOS)
	}
	var arch client.NodeArchitecture
	switch runtime.GOARCH {
	case "amd64":
		arch = client.NodeArchAMD64
	case "arm64":
		arch = client.NodeArchARM64
	default:
		return "", "", fmt.Errorf("unsupported node architecture %s", runtime.GOARCH)
	}
	return platform, arch, nil
}

func MachineFingerprint(values ...string) string {
	hash := sha256.New()
	for _, value := range values {
		hash.Write([]byte(value))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

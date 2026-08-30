package node

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/client"
)

type API interface {
	CreateNodeEnrollment(context.Context, string, string, string, client.CreateNodeEnrollmentRequest) (client.NodeEnrollmentSecret, error)
	ExchangeNodeEnrollment(context.Context, string, client.ExchangeNodeEnrollmentRequest) (client.ExchangeNodeEnrollmentResponse, error)
	SubmitNodeHeartbeat(context.Context, string, client.NodeHeartbeat) error
	ActivateNode(context.Context, string, string, client.NodeActivationRequest) (client.NodeActivationResponse, error)
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
	options.MachineFingerprint = canonicalMachineFingerprint(options.MachineFingerprint)
	if options.AccessToken == "" || options.WorkspaceID == "" || len(options.IdempotencyKey) < 8 || options.Name == "" || options.MachineFingerprint == "" || options.Profile.ID == "" {
		return EnrollResult{}, errors.New("node enrollment inputs are incomplete")
	}
	if !validMachineFingerprint(options.MachineFingerprint) {
		return EnrollResult{}, errors.New("machine fingerprint must be 64 lowercase hexadecimal characters")
	}
	if install && s.installer == nil {
		return EnrollResult{}, errors.New("privileged installer is unavailable")
	}
	release, err := s.state.AcquireRuntimeLock()
	if err != nil {
		return EnrollResult{}, fmt.Errorf("acquire node runtime transition lock: %w", err)
	}
	defer release()
	if install {
		if recovered, ok, err := s.recoverActivatedCapacity(ctx, options); ok || err != nil {
			return recovered, err
		}
		if err := s.retireRemovedEnrollment(ctx, options); err != nil {
			return EnrollResult{}, err
		}
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
		activationResponse, err := s.api.ActivateNode(ctx, proof, "node-activate-"+receipt.ReceiptID, activation)
		if err != nil {
			return result, fmt.Errorf("activate installed node: %w", err)
		}
		activated := activationResponse.Node
		if err := client.ValidateNode(activated); err != nil || activated.ID != response.Plan.NodeID || activated.LifecycleState != "active" || activated.TrustState != "verified" || activated.KubernetesBinding == nil || *activated.KubernetesBinding != *state.KubernetesBinding {
			return result, errors.New("activation response differs from the installed node")
		}
		receiptDigest, err := client.NodeInstallReceiptDigest(receipt)
		if err != nil || client.VerifyNodeActivationGrant(activationResponse.ActivationGrant, state.Pin.PlanSigningKey, response.Plan.NodeID, response.Plan.PlanID, receiptDigest, *state.KubernetesBinding) != nil {
			return result, errors.New("activation grant differs from the installed node")
		}
		state.ActivationGrant = &activationResponse.ActivationGrant
		state.UpdatedAt = nowString(s.now())
		if err := s.state.SaveRuntime(state); err != nil {
			return result, fmt.Errorf("persist server-authorized activation grant before capacity release: %w", err)
		}
		result.State = state
		releasedBinding, err := s.installer.ReleaseNodeCapacity(ctx, response.Plan, receipt, activationResponse.ActivationGrant)
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

func validMachineFingerprint(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func canonicalMachineFingerprint(value string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(value)), "sha256:")
}

func (s *Service) retireRemovedEnrollment(ctx context.Context, options EnrollOptions) error {
	runtimeState, runtimeErr := s.state.LoadRuntime()
	runtimeExists := runtimeErr == nil && runtimeState.SchemaVersion != 0
	if runtimeErr != nil && !errors.Is(runtimeErr, os.ErrNotExist) {
		return fmt.Errorf("load prior node runtime before enrollment: %w", runtimeErr)
	}
	pin, pinErr := s.state.LoadPin()
	pinExists := pinErr == nil
	if pinErr != nil && !errors.Is(pinErr, os.ErrNotExist) {
		return fmt.Errorf("load prior node pin before enrollment: %w", pinErr)
	}
	if !runtimeExists && !pinExists {
		return nil
	}
	if pinExists && sameEnrollmentIntent(pin, options) {
		if runtimeExists && !samePin(runtimeState.Pin, pin) {
			return errors.New("matching enrollment pin differs from its verified runtime state")
		}
		return nil
	}
	if !runtimeExists {
		return errors.New("different enrollment pin lacks cryptographically verified removed runtime state")
	}
	if pinExists && !samePin(runtimeState.Pin, pin) {
		return errors.New("prior node runtime differs from its enrollment pin")
	}
	if err := verifyPinnedRetirementRuntime(runtimeState); err != nil {
		return errors.New("prior node runtime cannot authorize enrollment retirement")
	}
	if err := s.installer.RetireRemovedEnrollment(ctx, runtimeState.Exchange.Plan); err != nil {
		return err
	}
	retirer, ok := s.state.(interface{ RetireEnrollmentState(RuntimeState) error })
	if !ok {
		return errors.New("local removed-enrollment retirer is unavailable")
	}
	if err := retirer.RetireEnrollmentState(runtimeState); err != nil {
		return fmt.Errorf("retire removed local enrollment state: %w", err)
	}
	return nil
}

func verifyPinnedRetirementRuntime(runtimeState RuntimeState) error {
	if err := client.ValidateExchangeNodeEnrollmentResponse(runtimeState.Exchange); err != nil {
		return err
	}
	plan, pin := runtimeState.Exchange.Plan, runtimeState.Pin
	if pin.SchemaVersion != 1 || plan.WorkspaceID != pin.WorkspaceID || plan.EnrollmentID != pin.EnrollmentID || plan.IdempotencyKey != pin.IdempotencyKey || plan.Hostname != pin.Hostname || plan.Target.MachineFingerprint != pin.MachineFingerprint || plan.InstallProfile != pin.ProfileID || plan.SigningKeyID != pin.PlanSigningKey.KeyID {
		return errors.New("retired plan differs from its pinned enrollment trust")
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(pin.PlanSigningKey.PublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return errors.New("retired plan signer public key is invalid")
	}
	fingerprint, err := client.NodePublicKeyFingerprint(ed25519.PublicKey(publicKey))
	if err != nil || fingerprint != pin.PlanSigningKey.Fingerprint {
		return errors.New("retired plan signer fingerprint differs from pin")
	}
	digest, err := client.NodeInstallPlanDigest(plan)
	signature, signatureErr := base64.RawURLEncoding.DecodeString(plan.Signature)
	if err != nil || signatureErr != nil || digest != plan.Digest || !ed25519.Verify(ed25519.PublicKey(publicKey), []byte("blazn-node-install-plan-v1\n"+digest), signature) {
		return errors.New("retired plan signature differs from pin")
	}
	return nil
}

func sameEnrollmentIntent(pin EnrollmentPin, options EnrollOptions) bool {
	return pin.WorkspaceID == options.WorkspaceID && pin.IdempotencyKey == options.IdempotencyKey && pin.Hostname == options.Name && pin.MachineFingerprint == options.MachineFingerprint && pin.ProfileID == options.Profile.ID && pin.ProfilePath == options.ProfilePath
}

func (s *Service) recoverActivatedCapacity(ctx context.Context, options EnrollOptions) (EnrollResult, bool, error) {
	state, err := s.state.LoadRuntime()
	if err != nil || state.KubernetesBinding == nil {
		return EnrollResult{}, false, nil
	}
	plan := state.Exchange.Plan
	if state.Pin.WorkspaceID != options.WorkspaceID || state.Pin.IdempotencyKey != options.IdempotencyKey || state.Pin.Hostname != options.Name || state.Pin.MachineFingerprint != options.MachineFingerprint || state.Pin.ProfileID != options.Profile.ID || plan.NodeID == "" || plan.PlanID == "" {
		return EnrollResult{}, false, nil
	}
	releaseRecoveryContext := s.installer.BindRecoveryContext(ctx, plan)
	defer releaseRecoveryContext()
	receipt, err := s.installer.LoadReceipt()
	if err != nil {
		return EnrollResult{State: state}, true, errors.New("persisted activation recovery state lacks its exact active receipt")
	}
	receiptDigest, err := client.NodeInstallReceiptDigest(receipt)
	if err != nil {
		return EnrollResult{State: state}, true, errors.New("persisted activation recovery evidence is invalid")
	}
	if state.ActivationGrant == nil {
		identity, identityErr := s.identities.LoadOrCreate()
		if identityErr != nil {
			return EnrollResult{State: state}, true, fmt.Errorf("load node identity for activation recovery: %w", identityErr)
		}
		expectedVersion := int64(1)
		if plan.Mode == client.NodeModeFresh {
			expectedVersion = 2
		}
		activation := client.NodeActivationRequest{ExpectedVersion: expectedVersion, Receipt: receipt, KubernetesBinding: *state.KubernetesBinding}
		proof, proofErr := nodeProof(identity.PrivateKey, "blazn-node-activation-v1", activation)
		if proofErr != nil {
			return EnrollResult{State: state}, true, proofErr
		}
		response, activationErr := s.api.ActivateNode(ctx, proof, "node-activate-"+receipt.ReceiptID, activation)
		if activationErr != nil {
			return EnrollResult{State: state, Installed: true, Receipt: &receipt}, true, fmt.Errorf("recover committed node activation: %w", activationErr)
		}
		if err := client.ValidateNode(response.Node); err != nil || response.Node.ID != plan.NodeID || response.Node.LifecycleState != "active" || response.Node.TrustState != "verified" || response.Node.KubernetesBinding == nil || *response.Node.KubernetesBinding != *state.KubernetesBinding || client.VerifyNodeActivationGrant(response.ActivationGrant, state.Pin.PlanSigningKey, plan.NodeID, plan.PlanID, receiptDigest, *state.KubernetesBinding) != nil {
			return EnrollResult{State: state, Installed: true, Receipt: &receipt}, true, errors.New("replayed activation response differs from persisted install evidence")
		}
		state.ActivationGrant = &response.ActivationGrant
		state.UpdatedAt = nowString(s.now())
		if err := s.state.SaveRuntime(state); err != nil {
			return EnrollResult{State: state, Installed: true, Receipt: &receipt}, true, fmt.Errorf("persist replayed activation grant before capacity recovery: %w", err)
		}
	}
	if client.VerifyNodeActivationGrant(*state.ActivationGrant, state.Pin.PlanSigningKey, plan.NodeID, plan.PlanID, receiptDigest, state.ActivationGrant.KubernetesBinding) != nil || state.ActivationGrant.KubernetesBinding.ClusterID != state.KubernetesBinding.ClusterID || state.ActivationGrant.KubernetesBinding.NodeName != state.KubernetesBinding.NodeName || state.ActivationGrant.KubernetesBinding.NodeUID != state.KubernetesBinding.NodeUID {
		return EnrollResult{State: state}, true, errors.New("persisted activation recovery evidence is invalid")
	}
	released, err := s.installer.RecoverActivatedCapacity(ctx, plan, receipt, *state.ActivationGrant, state.ActivationGrant.KubernetesBinding)
	if err != nil {
		return EnrollResult{State: state, Installed: true, Receipt: &receipt}, true, fmt.Errorf("recover activated node capacity: %w", err)
	}
	state.KubernetesBinding = released
	state.UpdatedAt = nowString(s.now())
	if err := s.state.SaveRuntime(state); err != nil {
		return EnrollResult{State: state, Installed: true, Receipt: &receipt}, true, fmt.Errorf("persist recovered Kubernetes binding: %w", err)
	}
	if err := s.installer.FinalizeServiceState(ctx, plan); err != nil {
		return EnrollResult{State: state, Installed: true, Receipt: &receipt}, true, fmt.Errorf("finalize recovered daemon-owned node state: %w", err)
	}
	return EnrollResult{State: state, Installed: true, Receipt: &receipt}, true, nil
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

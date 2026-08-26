package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/blazncloud/blazn/internal/client"
)

const RootInstallAuthoritySchema = "blazn.dev/node-root-install-authority/v1"

var (
	bootstrapDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	bootstrapTokenPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{43,128}$`)
)

// BootstrapAuthorization is an in-memory handoff to the privileged platform.
// Token is deliberately excluded from JSON and redacted from String/GoString.
type BootstrapAuthorization struct {
	EnrollmentID       string                                `json:"enrollmentId"`
	Token              string                                `json:"-"`
	MachineFingerprint string                                `json:"machineFingerprint"`
	NodePublicKey      string                                `json:"nodePublicKey"`
	Platform           client.NodePlatform                   `json:"platform"`
	Architecture       client.NodeArchitecture               `json:"architecture"`
	KubernetesBinding  *client.KubernetesBinding             `json:"kubernetesBinding,omitempty"`
	PlanSigningKey     client.NodePlanSigningKey             `json:"planSigningKey"`
	Expected           client.ExchangeNodeEnrollmentResponse `json:"expected"`
	ProfileID          string                                `json:"profileId"`
	ProfilePath        string                                `json:"profilePath"`
}

func (BootstrapAuthorization) String() string { return "node bootstrap authorization [REDACTED]" }
func (BootstrapAuthorization) GoString() string {
	return "node.BootstrapAuthorization{Token:[REDACTED]}"
}

func (a BootstrapAuthorization) Validate() error {
	if !bootstrapTokenPattern.MatchString(a.Token) || a.EnrollmentID == "" || a.MachineFingerprint == "" || a.NodePublicKey == "" || a.ProfileID == "" || !filepath.IsAbs(a.ProfilePath) || filepath.Clean(a.ProfilePath) != a.ProfilePath {
		return errors.New("bootstrap authorization is incomplete")
	}
	if err := client.ValidateExchangeNodeEnrollmentResponse(a.Expected); err != nil {
		return fmt.Errorf("bootstrap expected exchange: %w", err)
	}
	plan := a.Expected.Plan
	if plan.EnrollmentID != a.EnrollmentID || plan.InstallProfile != a.ProfileID || plan.Target.MachineFingerprint != a.MachineFingerprint || plan.Target.Platform != a.Platform || plan.Target.Architecture != a.Architecture || plan.SigningKeyID != a.PlanSigningKey.KeyID {
		return errors.New("bootstrap authorization does not bind its expected plan")
	}
	if plan.Mode == client.NodeModeFresh && a.KubernetesBinding != nil {
		return errors.New("fresh bootstrap authorization cannot pre-bind Kubernetes")
	}
	if plan.Mode == client.NodeModeAdopt && (a.KubernetesBinding == nil || a.KubernetesBinding.ClusterID != plan.Cluster.ID || a.KubernetesBinding.NodeName != plan.Hostname || a.KubernetesBinding.NodeUID == "" || a.KubernetesBinding.ResourceVersion == "") {
		return errors.New("adopt bootstrap authorization does not match the signed Kubernetes binding")
	}
	planKey, err := base64.RawURLEncoding.DecodeString(a.PlanSigningKey.PublicKey)
	if err != nil || len(planKey) != ed25519.PublicKeySize {
		return errors.New("bootstrap plan signer public key is invalid")
	}
	planFingerprint, err := client.NodePublicKeyFingerprint(ed25519.PublicKey(planKey))
	if err != nil || planFingerprint != a.PlanSigningKey.Fingerprint {
		return errors.New("bootstrap plan signer fingerprint is inconsistent")
	}
	nodeKey, err := base64.RawURLEncoding.DecodeString(a.NodePublicKey)
	if err != nil || len(nodeKey) != ed25519.PublicKeySize {
		return errors.New("bootstrap node public key is invalid")
	}
	nodeFingerprint, err := client.NodePublicKeyFingerprint(ed25519.PublicKey(nodeKey))
	if err != nil || nodeFingerprint != a.Expected.Identity.PublicKeyFingerprint {
		return errors.New("bootstrap node identity fingerprint is inconsistent")
	}
	return nil
}

type BootstrapAuthorizer interface {
	AuthorizeBootstrap(context.Context, BootstrapAuthorization) error
}

// RootInstallAuthority is the token-free, root-owned result of an independently
// authenticated enrollment exchange. Concrete persistence is platform-owned.
type RootInstallAuthority struct {
	SchemaVersion      string                        `json:"schemaVersion"`
	Plan               client.NodeInstallPlan        `json:"plan"`
	Identity           client.NodeEnrollmentIdentity `json:"identity"`
	PlanSigningKey     client.NodePlanSigningKey     `json:"planSigningKey"`
	NodePublicKey      string                        `json:"nodePublicKey"`
	KubernetesBinding  *client.KubernetesBinding     `json:"kubernetesBinding"`
	JoinIntent         *RootJoinIntent               `json:"joinIntent"`
	ProfileID          string                        `json:"profileId"`
	ProfileSHA256      string                        `json:"profileSha256"`
	ProfileOwnerUID    int64                         `json:"profileOwnerUid"`
	ControlPlaneOrigin string                        `json:"controlPlaneOrigin"`
	AuthorizedAt       string                        `json:"authorizedAt"`
	Digest             string                        `json:"digest"`
}

type RootJoinIntent struct {
	ClusterID        string `json:"clusterId"`
	ExpectedNodeName string `json:"expectedNodeName"`
	BootstrapTaint   string `json:"bootstrapTaint"`
	WorkerOnly       bool   `json:"workerOnly"`
	StartedAt        string `json:"startedAt"`
}

type RootInstallAuthorityTrust struct {
	Now           time.Time
	Profile       client.NodeTrustedInstallProfile
	ProfileSHA256 string
}

func ValidateRootInstallAuthority(authority RootInstallAuthority) error {
	if authority.SchemaVersion != RootInstallAuthoritySchema || authority.ProfileID == "" || authority.Plan.InstallProfile != authority.ProfileID || !bootstrapDigestPattern.MatchString(authority.ProfileSHA256) || authority.ProfileOwnerUID < 0 || !validControlPlaneOrigin(authority.ControlPlaneOrigin) {
		return errors.New("root install authority binding is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, authority.AuthorizedAt); err != nil {
		return errors.New("root install authority timestamp is invalid")
	}
	if err := client.ValidateExchangeNodeEnrollmentResponse(client.ExchangeNodeEnrollmentResponse{Plan: authority.Plan, Identity: authority.Identity}); err != nil {
		return err
	}
	if authority.Plan.SigningKeyID != authority.PlanSigningKey.KeyID {
		return errors.New("root install authority plan signer key ID is invalid")
	}
	if authority.Plan.Mode == client.NodeModeFresh && authority.KubernetesBinding != nil {
		if authority.JoinIntent == nil {
			return errors.New("fresh root install authority binding lacks join intent")
		}
	}
	if authority.Plan.Mode == client.NodeModeAdopt && (authority.KubernetesBinding == nil || authority.KubernetesBinding.ClusterID != authority.Plan.Cluster.ID || authority.KubernetesBinding.NodeName != authority.Plan.Hostname || authority.KubernetesBinding.NodeUID == "" || authority.KubernetesBinding.ResourceVersion == "") {
		return errors.New("root install authority Kubernetes binding is invalid")
	}
	if authority.JoinIntent != nil {
		intent := authority.JoinIntent
		if authority.Plan.Mode != client.NodeModeFresh || intent.ClusterID != authority.Plan.Cluster.ID || intent.ExpectedNodeName != authority.Plan.Hostname || intent.BootstrapTaint != authority.Plan.Cluster.BootstrapTaint || !intent.WorkerOnly {
			return errors.New("root install authority join intent is invalid")
		}
		if _, err := time.Parse(time.RFC3339Nano, intent.StartedAt); err != nil {
			return errors.New("root install authority join intent timestamp is invalid")
		}
	}
	planKey, err := base64.RawURLEncoding.DecodeString(authority.PlanSigningKey.PublicKey)
	if err != nil || len(planKey) != ed25519.PublicKeySize {
		return errors.New("root install authority plan signer is invalid")
	}
	planFingerprint, err := client.NodePublicKeyFingerprint(ed25519.PublicKey(planKey))
	if err != nil || planFingerprint != authority.PlanSigningKey.Fingerprint {
		return errors.New("root install authority plan signer fingerprint is invalid")
	}
	nodeKey, err := base64.RawURLEncoding.DecodeString(authority.NodePublicKey)
	if err != nil || len(nodeKey) != ed25519.PublicKeySize {
		return errors.New("root install authority node public key is invalid")
	}
	nodeFingerprint, err := client.NodePublicKeyFingerprint(ed25519.PublicKey(nodeKey))
	if err != nil || nodeFingerprint != authority.Identity.PublicKeyFingerprint || nodeFingerprint != authority.Plan.Target.NodePublicKeyFingerprint {
		return errors.New("root install authority node public key fingerprint is invalid")
	}
	digest, err := RootInstallAuthorityDigest(authority)
	if err != nil || authority.Digest != digest {
		return errors.New("root install authority digest is invalid")
	}
	return nil
}

func DecodeRootInstallAuthority(encoded []byte) (RootInstallAuthority, error) {
	var authority RootInstallAuthority
	var fields map[string]json.RawMessage
	if json.Unmarshal(encoded, &fields) != nil || len(fields) != 13 {
		return authority, errors.New("root install authority fields are invalid")
	}
	for _, name := range []string{"schemaVersion", "plan", "identity", "planSigningKey", "nodePublicKey", "kubernetesBinding", "joinIntent", "profileId", "profileSha256", "profileOwnerUid", "controlPlaneOrigin", "authorizedAt", "digest"} {
		if fields[name] == nil {
			return RootInstallAuthority{}, errors.New("root install authority fields are incomplete")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&authority); err != nil {
		return authority, errors.New("root install authority is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return RootInstallAuthority{}, errors.New("root install authority has trailing data")
	}
	if err := ValidateRootInstallAuthority(authority); err != nil {
		return RootInstallAuthority{}, err
	}
	return authority, nil
}

func VerifyRootInstallAuthority(authority RootInstallAuthority, trust RootInstallAuthorityTrust) error {
	if err := ValidateRootInstallAuthority(authority); err != nil {
		return err
	}
	if trust.Now.IsZero() || trust.ProfileSHA256 != authority.ProfileSHA256 || trust.Profile.ID != authority.ProfileID || trust.Profile.ControlPlaneOrigin != authority.ControlPlaneOrigin {
		return errors.New("root install authority trust input is invalid")
	}
	identityExpiry, err := time.Parse(time.RFC3339, authority.Identity.ExpiresAt)
	if err != nil || !trust.Now.Before(identityExpiry) {
		return errors.New("root install authority identity is expired")
	}
	planKey, _ := base64.RawURLEncoding.DecodeString(authority.PlanSigningKey.PublicKey)
	nodeKey, _ := base64.RawURLEncoding.DecodeString(authority.NodePublicKey)
	plan := authority.Plan
	verification := client.NodeInstallPlanTrust{Now: trust.Now, Keyring: client.NodeSigningKeyring{authority.PlanSigningKey.KeyID: ed25519.PublicKey(planKey)}, WorkspaceID: plan.WorkspaceID, EnrollmentID: plan.EnrollmentID, NodeID: plan.NodeID, Hostname: plan.Hostname, MachineFingerprint: plan.Target.MachineFingerprint, NodePublicKey: ed25519.PublicKey(nodeKey), Platform: plan.Target.Platform, Architecture: plan.Target.Architecture, IdempotencyKey: plan.IdempotencyKey, Profile: trust.Profile}
	if err := client.VerifyNodeInstallPlan(plan, verification); err != nil {
		return fmt.Errorf("verify root-authorized install plan: %w", err)
	}
	return nil
}

func RootInstallAuthorityDigest(authority RootInstallAuthority) (string, error) {
	authority.Digest = ""
	canonical, err := canonicalJSON(authority)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("blazn-node-root-install-authority-v1\n"), canonical...))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func validControlPlaneOrigin(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery || parsed.String() != value || strings.HasSuffix(parsed.Host, ":") {
		return false
	}
	if parsed.Port() != "" {
		port, err := strconv.Atoi(parsed.Port())
		return err == nil && port >= 1 && port <= 65535
	}
	return true
}

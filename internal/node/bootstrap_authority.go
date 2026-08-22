package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/KingJammin/blazn/internal/client"
)

const RootInstallAuthoritySchema = "blazn.dev/node-root-install-authority/v1"

var bootstrapDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

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
	if len(a.Token) < 43 || len(a.Token) > 128 || a.EnrollmentID == "" || a.MachineFingerprint == "" || a.NodePublicKey == "" || a.ProfileID == "" || a.ProfilePath == "" {
		return errors.New("bootstrap authorization is incomplete")
	}
	if err := client.ValidateExchangeNodeEnrollmentResponse(a.Expected); err != nil {
		return fmt.Errorf("bootstrap expected exchange: %w", err)
	}
	plan := a.Expected.Plan
	if plan.EnrollmentID != a.EnrollmentID || plan.InstallProfile != a.ProfileID || plan.Target.MachineFingerprint != a.MachineFingerprint || plan.Target.Platform != a.Platform || plan.Target.Architecture != a.Architecture || plan.SigningKeyID != a.PlanSigningKey.KeyID {
		return errors.New("bootstrap authorization does not bind its expected plan")
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
	ProfileID          string                        `json:"profileId"`
	ProfileSHA256      string                        `json:"profileSha256"`
	ControlPlaneOrigin string                        `json:"controlPlaneOrigin"`
	AuthorizedAt       string                        `json:"authorizedAt"`
	Digest             string                        `json:"digest"`
}

func ValidateRootInstallAuthority(authority RootInstallAuthority) error {
	if authority.SchemaVersion != RootInstallAuthoritySchema || authority.ProfileID == "" || authority.Plan.InstallProfile != authority.ProfileID || !bootstrapDigestPattern.MatchString(authority.ProfileSHA256) || !validControlPlaneOrigin(authority.ControlPlaneOrigin) {
		return errors.New("root install authority binding is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, authority.AuthorizedAt); err != nil {
		return errors.New("root install authority timestamp is invalid")
	}
	if err := client.ValidateExchangeNodeEnrollmentResponse(client.ExchangeNodeEnrollmentResponse{Plan: authority.Plan, Identity: authority.Identity}); err != nil {
		return err
	}
	digest, err := RootInstallAuthorityDigest(authority)
	if err != nil || authority.Digest != digest {
		return errors.New("root install authority digest is invalid")
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
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Path == "" && parsed.RawPath == "" && parsed.RawQuery == "" && parsed.Fragment == "" && !parsed.ForceQuery && parsed.String() == value
}
